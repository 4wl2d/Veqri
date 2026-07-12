package com.veqri.android.data

import com.veqri.android.call.CallLifecycleController
import com.veqri.android.media.AudioRouteController
import com.veqri.android.media.MediaSessionConfig
import com.veqri.android.media.MediaTransportState
import com.veqri.android.media.NoOpSpeechPlayback
import com.veqri.android.media.SpeechPlayback
import com.veqri.android.media.SpeechPlaybackState
import com.veqri.android.media.VoiceMediaTransport
import com.veqri.android.network.CoreCommand
import com.veqri.android.network.CoreEvent
import com.veqri.android.network.CoreTransport
import com.veqri.android.network.CommandCommitException
import com.veqri.android.network.CommandCommitFailureKind
import com.veqri.android.network.CredentialRotationException
import com.veqri.android.network.CredentialRotationFailureKind
import com.veqri.android.network.IdSource
import com.veqri.android.network.PairingRequest
import java.util.UUID
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.collectLatest
	import kotlinx.coroutines.flow.first
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
	import kotlinx.coroutines.withTimeout
	import kotlinx.coroutines.sync.Mutex
	import kotlinx.coroutines.sync.withLock

class VeqriRepository(
    private val scope: CoroutineScope,
    private val transport: CoreTransport,
    private val credentialStore: DeviceCredentialStore,
    private val preferenceStore: ClientPreferenceStore,
    private val cache: ConversationCache,
    private val mediaTransport: VoiceMediaTransport,
    private val audioRoutes: AudioRouteController,
    private val calls: CallLifecycleController,
    private val speechPlayback: SpeechPlayback = NoOpSpeechPlayback(),
    private val idSource: IdSource = IdSource { UUID.randomUUID().toString() },
) {
    private val mutableSnapshot = MutableStateFlow(ClientSnapshot())
    private var activeMediaSessionId: String? = null
    private var readyMediaSessionId: String? = null
	private var latestSpeechPlaybackState = SpeechPlaybackState()
	private var latestPreferences = ClientPreferences(
        coreBaseUrl = "",
        retainTranscript = true,
        preferPushToTalk = false,
	)
	private val cacheMutex = Mutex()
	private val credentialRotationMutex = Mutex()
	private val retentionCommandMutex = Mutex()
	@Volatile
	private var retentionOutcomeUnknown = false

    val snapshot: StateFlow<ClientSnapshot> = mutableSnapshot.asStateFlow()

    init {
        scope.launch { transport.events.collect(::handleEvent) }
        scope.launch {
            transport.connection.collect { connection ->
				if ((connection is ConnectionStatus.Connecting || connection is ConnectionStatus.Reconnecting) &&
					transport.isAwaitingAuthoritativeSnapshot()
				) {
					markRetentionAwaitingSnapshot()
				}
                mutableSnapshot.update { current ->
                    val updatedVoice = when {
                        connection is ConnectionStatus.Reconnecting && current.voiceSession?.phase !in
                            setOf(null, DialogPhase.ENDED) -> {
                            current.voiceSession?.copy(phase = DialogPhase.RECONNECTING)
                        }
                        connection is ConnectionStatus.Connected &&
                            current.voiceSession?.phase == DialogPhase.RECONNECTING -> {
                            current.voiceSession.copy(phase = DialogPhase.LISTENING)
                        }
                        else -> current.voiceSession
                    }
                    current.copy(connection = connection, voiceSession = updatedVoice)
                }
            }
        }
        scope.launch {
            preferenceStore.preferences.collectLatest { preferences ->
                // Retention is committed only by an acknowledged Core command or
                // authoritative snapshot. Unrelated DataStore emissions must not
                // overwrite that in-memory authority with an older local value.
                cacheMutex.withLock {
                    latestPreferences = preferences.copy(
                        retainTranscript = latestPreferences.retainTranscript,
                    )
                }
            }
        }
        scope.launch {
            audioRoutes.availableRoutes.collect { routes ->
                mutableSnapshot.update { it.copy(availableAudioRoutes = routes) }
            }
        }
        scope.launch {
            mediaTransport.state.collect { mediaState ->
                applyMediaState(mediaState)
            }
        }
        scope.launch {
            speechPlayback.state.collect { playbackState ->
                applySpeechPlaybackState(playbackState)
            }
        }
        scope.launch { bootstrap() }
    }

    suspend fun pair(coreBaseUrl: String, oneTimeCode: String, deviceName: String) {
        mutableSnapshot.update {
            it.copy(pairingInProgress = true, pairingError = null, errorMessage = null)
        }
        val retainTranscript = runCatching { preferenceStore.preferences.first().retainTranscript }
            .getOrElse { cacheMutex.withLock { latestPreferences.retainTranscript } }
        val result = transport.pair(
            PairingRequest(
                coreBaseUrl = coreBaseUrl,
                oneTimeCode = oneTimeCode,
                deviceName = deviceName,
                retainTranscript = retainTranscript,
            ),
        )
        result.fold(
            onSuccess = { credential ->
                runCatching {
                    credentialStore.save(credential)
                    preferenceStore.setCoreBaseUrl(credential.coreBaseUrl)
                    markRetentionAwaitingSnapshot()
                    mutableSnapshot.update {
                        it.copy(
                            isPaired = true,
                            pairingInProgress = false,
                            pairingError = null,
                            credentialKeyVersion = credential.keyVersion,
                            credentialRotation = CredentialRotationState(),
                        )
                    }
                    transport.connect(credential)
                    withTimeout(INITIAL_SNAPSHOT_TIMEOUT_MILLIS) {
                        transport.connection.first { it is ConnectionStatus.Connected }
                        snapshot.first { it.retentionStateAuthoritative }
                    }
                }.onFailure { error ->
                    mutableSnapshot.update {
                        it.copy(
                            pairingInProgress = false,
                            pairingError = error.safeMessage("Pairing completed but the device credential could not be stored."),
                        )
                    }
                }
            },
            onFailure = { error ->
                mutableSnapshot.update {
                    it.copy(
                        pairingInProgress = false,
                        pairingError = error.safeMessage("Pairing failed."),
                    )
                }
            },
        )
    }

    suspend fun sendText(text: String) = executeCommand {
		retentionCommandMutex.withLock {
			requireRetentionOutcomeKnown()
			val trimmed = text.trim()
			require(trimmed.isNotEmpty()) { "Enter a message before sending." }
			val retainTranscript = cacheMutex.withLock { latestPreferences.retainTranscript }
			transport.send(
				CoreCommand.SendText(
					commandId = idSource.nextId(),
					conversationId = mutableSnapshot.value.conversationId,
					text = trimmed,
					retainTranscript = retainTranscript,
				),
			)
		}
    }

	suspend fun startCall() = executeCommand {
		retentionCommandMutex.withLock {
			requireRetentionOutcomeKnown()
			val retainTranscript = cacheMutex.withLock { latestPreferences.retainTranscript }
			transport.send(CoreCommand.StartCall(idSource.nextId(), retainTranscript))
		}
	}

	suspend fun simulateIncomingCall() = executeCommand {
		retentionCommandMutex.withLock {
			requireRetentionOutcomeKnown()
			val retainTranscript = cacheMutex.withLock { latestPreferences.retainTranscript }
			transport.send(CoreCommand.SimulateIncomingCall(idSource.nextId(), retainTranscript))
		}
    }

    suspend fun answerCall(sessionId: String) = executeCommand {
        transport.send(CoreCommand.AnswerCall(idSource.nextId(), sessionId))
    }

    suspend fun declineCall(sessionId: String) = executeCommand {
        transport.send(CoreCommand.DeclineCall(idSource.nextId(), sessionId))
    }

    suspend fun endCall(sessionId: String) = executeCommand {
        transport.send(CoreCommand.EndCall(idSource.nextId(), sessionId))
        runCatching { speechPlayback.stop() }
        runCatching { mediaTransport.stop() }
        audioRoutes.stop()
        activeMediaSessionId = null
        readyMediaSessionId = null
        calls.endActiveCall()
    }

    suspend fun toggleMute(sessionId: String) = executeCommand {
        val muted = !(mutableSnapshot.value.voiceSession?.isMuted ?: false)
        mediaTransport.setMuted(muted)
        transport.send(CoreCommand.SetMuted(idSource.nextId(), sessionId, muted))
    }

    suspend fun setPushToTalk(sessionId: String, enabled: Boolean) = executeCommand {
        preferenceStore.setPreferPushToTalk(enabled)
        mediaTransport.setMuted(enabled)
        transport.send(CoreCommand.SetPushToTalk(idSource.nextId(), sessionId, enabled))
        transport.send(CoreCommand.SetMuted(idSource.nextId(), sessionId, enabled))
    }

    suspend fun setPushToTalkPressed(sessionId: String, pressed: Boolean) = executeCommand {
        if (mutableSnapshot.value.voiceSession?.isPushToTalk != true) return@executeCommand
        val muted = !pressed
        mediaTransport.setMuted(muted)
        transport.send(CoreCommand.SetMuted(idSource.nextId(), sessionId, muted))
    }

    suspend fun selectAudioRoute(sessionId: String, route: AudioRouteKind) = executeCommand {
        mediaTransport.selectAudioRoute(route)
        transport.send(CoreCommand.SelectAudioRoute(idSource.nextId(), sessionId, route))
    }

    suspend fun interruptTts(sessionId: String) = executeCommand {
        runCatching { speechPlayback.stop() }
        runCatching { mediaTransport.interruptPlayback() }
        transport.send(CoreCommand.InterruptTts(idSource.nextId(), sessionId))
    }

    suspend fun resolveApproval(approvalId: String, approved: Boolean) = executeCommand {
        transport.send(CoreCommand.ResolveApproval(idSource.nextId(), approvalId, approved))
    }

    suspend fun cancelTask(taskId: String) = executeCommand {
        transport.send(CoreCommand.CancelTask(idSource.nextId(), taskId))
    }

    suspend fun retryTask(taskId: String) = executeCommand {
        require(mutableSnapshot.value.tasks[taskId]?.canRetry == true) {
            "This task is not eligible for retry."
        }
        transport.send(CoreCommand.RetryTask(idSource.nextId(), taskId))
    }

	suspend fun rotateCredential() {
		credentialRotationMutex.withLock {
			val active = runCatching { credentialStore.read() }.getOrElse { error ->
				updateCredentialRotation(
					CredentialRotationPhase.FAILED,
					message = error.safeMessage("The active device credential is unavailable."),
				)
				return@withLock
			} ?: run {
				requirePairingAfterRotationFailure("No active device credential is available. Pair this device again.")
				return@withLock
			}
			val candidate = runCatching { credentialStore.readRotationCandidate() }.getOrElse {
				recoverCorruptRotationState(active)
				return@withLock
			}
			if (candidate == null) {
				prepareFreshCredentialRotation(active, retryAfterExpiry = true)
			} else {
				confirmStoredCredentialRotation(active, candidate, retryAfterExpiry = true)
			}
		}
	}

	suspend fun setRetainTranscript(value: Boolean) {
		retentionCommandMutex.withLock {
			if (retentionOutcomeUnknown || transport.isAwaitingAuthoritativeSnapshot()) {
				publishError("Wait for Core to reconnect and reconcile the privacy preference before changing it again.")
				return@withLock
			}
			val conversationId = mutableSnapshot.value.conversationId
			mutableSnapshot.update { it.copy(retentionChangeInProgress = true) }
			try {
				transport.sendAwaitingCommit(
					CoreCommand.SetTranscriptRetention(idSource.nextId(), conversationId, value),
				)
			} catch (error: CancellationException) {
				markRetentionAwaitingSnapshot()
				throw error
			} catch (error: Exception) {
				val outcomeUnknown = error is CommandCommitException &&
					error.kind == CommandCommitFailureKind.OUTCOME_UNKNOWN
				retentionOutcomeUnknown = outcomeUnknown
				mutableSnapshot.update {
					it.copy(
						retentionChangeInProgress = false,
						retentionStateAuthoritative = !outcomeUnknown,
					)
				}
				publishError(error.safeMessage("The privacy preference could not be saved."))
				return@withLock
			}

			retentionOutcomeUnknown = false
			val localFailure = cacheMutex.withLock {
				latestPreferences = latestPreferences.copy(retainTranscript = value)
				val cacheFailure = if (!value) {
					runCatching {
						if (conversationId != null) cache.deleteConversation(conversationId) else cache.clearTranscriptContent()
					}.exceptionOrNull()
				} else {
					null
				}
				// Persist false even when the immediate scrub failed so bootstrap
				// retries the scrub before loading Room. Conversely, when DataStore
				// fails, the already-scrubbed cache remains safe to reload.
				val preferenceFailure = runCatching {
					preferenceStore.setRetainTranscript(value)
				}.exceptionOrNull()
				cacheFailure ?: preferenceFailure
			}
			mutableSnapshot.update { current ->
				current.copy(
					retainTranscript = value,
					retentionChangeInProgress = false,
					retentionStateAuthoritative = true,
					messages = if (!value) {
						current.messages.filterNot { conversationId == null || it.conversationId == conversationId }
					} else {
						current.messages
					},
					tasks = if (!value) {
						current.tasks.mapValues { (_, task) ->
							if (conversationId == null || task.conversationId == conversationId) task.redactedForCache() else task
						}
					} else {
						current.tasks
					},
					partialTranscript = if (!value) "" else current.partialTranscript,
					finalTranscript = if (!value) "" else current.finalTranscript,
				)
			}
			if (localFailure != null) {
				publishError(localFailure.safeMessage("Core saved the privacy preference, but local privacy state could not be persisted."))
			}
		}
	}

    suspend fun forgetLocalDevice() {
        runCatching {
            transport.disconnect()
            credentialStore.clear()
            cache.clearAll()
            runCatching { speechPlayback.stop() }
            runCatching { mediaTransport.stop() }
            calls.endActiveCall()
        }.onSuccess {
            activeMediaSessionId = null
            readyMediaSessionId = null
            mutableSnapshot.value = ClientSnapshot()
        }.onFailure { publishError(it.safeMessage("The local device identity could not be cleared.")) }
    }

    fun clearError() {
        mutableSnapshot.update { it.copy(errorMessage = null, pairingError = null) }
    }

	private suspend fun bootstrap() {
		val bootstrapPreferences = runCatching { preferenceStore.preferences.first() }.getOrNull()
		if (bootstrapPreferences != null) {
			cacheMutex.withLock {
				latestPreferences = bootstrapPreferences
			}
			mutableSnapshot.update { it.copy(retainTranscript = bootstrapPreferences.retainTranscript) }
		}
		val credential = runCatching { credentialStore.read() }.getOrElse {
			publishError(it.safeMessage("The saved device credential is unavailable."))
			return
		}
		if (credential != null) {
			// Room is only a cache. A persisted preference can be stale when a
			// prior Core-acknowledged opt-out could not update DataStore, so never
			// expose paired transcript content before Core's first authoritative
			// snapshot. Redacted task status remains useful while offline.
			markRetentionAwaitingSnapshot()
			mutableSnapshot.update {
				it.copy(isPaired = true, credentialKeyVersion = credential.keyVersion)
			}
		}
		val cached = runCatching {
			cacheMutex.withLock {
				if (credential != null || !latestPreferences.retainTranscript) {
					cache.clearTranscriptContent()
				}
				cache.load()
			}
		}.onFailure {
			publishError(it.safeMessage("Local conversation history could not be loaded safely."))
		}.getOrNull()
		if (cached != null) {
			mutableSnapshot.update {
				it.copy(
					messages = cached.messages,
					tasks = cached.tasks.associateBy(TaskRecord::id),
					conversationId = cached.messages.lastOrNull()?.conversationId,
				)
			}
		}
		if (credential == null) return
		val candidate = runCatching { credentialStore.readRotationCandidate() }.getOrElse {
			recoverCorruptRotationState(credential)
			return
		}
		if (candidate == null) {
			runCatching { transport.connect(credential) }
				.onFailure { publishError(it.safeMessage("The saved Veqri Core connection could not start.")) }
			return
		}
		credentialRotationMutex.withLock {
			updateCredentialRotation(
				CredentialRotationPhase.RECOVERING,
				candidate.keyVersion,
				"Resuming a credential rotation saved before the app stopped.",
			)
			confirmStoredCredentialRotation(credential, candidate, retryAfterExpiry = true)
		}
	}

    private suspend fun handleEvent(event: CoreEvent) {
        when (event) {
			is CoreEvent.AuthoritativeSnapshot -> applyAuthoritativeSnapshot(event)
			is CoreEvent.MessageAdded -> {
				mutableSnapshot.update { current ->
					current.copy(
						conversationId = if (event.message.conversationId == "system") {
							current.conversationId
						} else {
							event.message.conversationId
						},
                        messages = (current.messages.filterNot { it.id == event.message.id } + event.message)
                            .sortedBy(ConversationMessage::createdAtEpochMillis),
                    )
                }
				cacheMutex.withLock {
					if (latestPreferences.retainTranscript) cacheSafely { cache.upsert(event.message) }
				}
            }
            is CoreEvent.TaskChanged -> {
                val previous = mutableSnapshot.value.tasks[event.task.id]
				if (event.task.dismissed) {
					mutableSnapshot.update { current ->
						current.copy(tasks = current.tasks - event.task.id)
					}
					cacheMutex.withLock { cacheSafely { cache.deleteTask(event.task.id) } }
					return
				}
                mutableSnapshot.update { current ->
                    current.copy(tasks = current.tasks + (event.task.id to event.task))
                }
				cacheMutex.withLock {
					val cacheTask = if (latestPreferences.retainTranscript) event.task else event.task.redactedForCache()
					cacheSafely { cache.upsert(cacheTask) }
				}
                if (event.task.status == TaskStatus.COMPLETED && previous?.status != TaskStatus.COMPLETED) {
                    calls.publishTaskCompleted(event.task)
                }
            }
            is CoreEvent.ApprovalChanged -> {
                val previous = mutableSnapshot.value.approvals[event.approval.id]
                mutableSnapshot.update { current ->
                    current.copy(approvals = current.approvals + (event.approval.id to event.approval))
                }
                if (event.approval.status == ApprovalStatus.PENDING && previous?.status != ApprovalStatus.PENDING) {
                    calls.publishApprovalRequired(event.approval)
                }
            }
            is CoreEvent.PartialTranscript -> {
                mutableSnapshot.update {
                    it.copy(
                        conversationId = event.conversationId,
                        partialTranscript = event.text,
                    )
                }
            }
            is CoreEvent.FinalTranscript -> {
                mutableSnapshot.update {
                    it.copy(
                        conversationId = event.conversationId,
                        partialTranscript = "",
                        finalTranscript = event.text,
                    )
                }
            }
            is CoreEvent.IncomingCall -> {
                mutableSnapshot.update {
                    it.copy(conversationId = event.session.conversationId, voiceSession = event.session)
                }
                calls.publishIncomingCall(event.session)
            }
            is CoreEvent.VoiceChanged -> handleVoiceChanged(event.session)
            is CoreEvent.TtsChanged -> {
                if (!speechPlayback.handlesPlayback) {
                    mutableSnapshot.update { current ->
                        current.copy(
                            voiceSession = current.voiceSession?.copy(ttsPlayback = event.status),
                        )
                    }
                }
            }
            is CoreEvent.TtsSpeak -> {
                val session = mutableSnapshot.value.voiceSession
                if (session != null &&
                    session.id == event.sessionId &&
                    session.conversationId == event.conversationId &&
                    session.phase !in setOf(DialogPhase.ENDED, DialogPhase.FAILED)
                ) {
                    runCatching { speechPlayback.speak(session.id, event.text) }
                        .onFailure { publishError(it.safeMessage("Speech playback could not start.")) }
                }
            }
			is CoreEvent.CredentialRotationCommitted -> {
				completeCredentialRotationFromSocket(event.keyVersion)
			}
			is CoreEvent.CommandResult -> Unit
            is CoreEvent.ProtocolError -> publishError(event.safeMessage)
        }
    }

	private suspend fun prepareFreshCredentialRotation(
		active: DeviceCredential,
		retryAfterExpiry: Boolean,
	) {
		updateCredentialRotation(
			CredentialRotationPhase.PREPARING,
			active.keyVersion + 1,
			"Preparing a replacement while the current credential remains active.",
		)
		try {
			// Idempotent cancellation recovers a prior prepare response that may
			// have been lost before Android could persist its candidate.
			transport.cancelCredentialRotation(active)
			val candidate = transport.prepareCredentialRotation(active)
			try {
				credentialStore.saveRotationCandidate(candidate)
			} catch (storageError: Exception) {
				runCatching { transport.cancelCredentialRotation(active) }
				transport.armCredentialRotation(null)
				updateCredentialRotation(
					CredentialRotationPhase.FAILED,
					candidate.keyVersion,
					storageError.safeMessage("The replacement could not be secured; the current credential remains active."),
				)
				return
			}
			transport.armCredentialRotation(candidate)
			confirmStoredCredentialRotation(active, candidate, retryAfterExpiry)
		} catch (error: CredentialRotationException) {
			if (error.kind == CredentialRotationFailureKind.UNAUTHORIZED) {
				requirePairingAfterRotationFailure("The active credential is no longer authorized. Pair this device again.")
			} else {
				updateCredentialRotation(
					CredentialRotationPhase.FAILED,
					active.keyVersion + 1,
					rotationFailureMessage(error, candidateStored = false),
				)
			}
		} catch (error: Exception) {
			updateCredentialRotation(
				CredentialRotationPhase.FAILED,
				active.keyVersion + 1,
				error.safeMessage("Credential rotation could not be prepared. The current credential remains active."),
			)
		}
	}

	private suspend fun confirmStoredCredentialRotation(
		active: DeviceCredential,
		candidate: CredentialRotationCandidate,
		retryAfterExpiry: Boolean,
	) {
		if (candidate.deviceId != active.deviceId || candidate.coreBaseUrl != active.coreBaseUrl ||
			candidate.keyVersion <= active.keyVersion
		) {
			recoverCorruptRotationState(active)
			return
		}
		updateCredentialRotation(
			CredentialRotationPhase.CONFIRMING,
			candidate.keyVersion,
			"The replacement is secured. Waiting for Core to promote it.",
		)
		transport.armCredentialRotation(candidate)
		try {
			transport.confirmCredentialRotation(candidate)
			promoteCredentialAndConnect(candidate)
		} catch (error: CredentialRotationException) {
			when (error.kind) {
				CredentialRotationFailureKind.EXPIRED -> recoverExpiredCredentialRotation(
					active,
					candidate,
					retryAfterExpiry,
				)
				CredentialRotationFailureKind.UNAUTHORIZED -> recoverRejectedCredentialCandidate(active, candidate)
				else -> updateCredentialRotation(
					CredentialRotationPhase.FAILED,
					candidate.keyVersion,
					rotationFailureMessage(error, candidateStored = true),
				)
			}
		} catch (error: Exception) {
			updateCredentialRotation(
				CredentialRotationPhase.FAILED,
				candidate.keyVersion,
				error.safeMessage("Confirmation did not finish. The replacement is stored safely; tap Resume rotation."),
			)
		}
	}

	private suspend fun recoverExpiredCredentialRotation(
		active: DeviceCredential,
		candidate: CredentialRotationCandidate,
		retryAfterExpiry: Boolean,
	) {
		updateCredentialRotation(
			CredentialRotationPhase.RECOVERING,
			candidate.keyVersion,
			"The replacement expired. Restoring the current credential and clearing Core's pending state.",
		)
		transport.armCredentialRotation(null)
		runCatching { credentialStore.clearRotationCandidate() }.onFailure {
			updateCredentialRotation(
				CredentialRotationPhase.FAILED,
				candidate.keyVersion,
				"The expired replacement could not be cleared safely. Restart the app before retrying.",
			)
			return
		}
		try {
			transport.cancelCredentialRotation(active)
			transport.connect(active)
			if (retryAfterExpiry) {
				prepareFreshCredentialRotation(active, retryAfterExpiry = false)
			} else {
				updateCredentialRotation(
					CredentialRotationPhase.FAILED,
					active.keyVersion + 1,
					"The expired rotation was cancelled. Tap Rotate credential to try again.",
				)
			}
		} catch (error: CredentialRotationException) {
			if (error.kind == CredentialRotationFailureKind.UNAUTHORIZED) {
				requirePairingAfterRotationFailure("Neither saved credential is authorized. Pair this device again.")
			} else {
				runCatching { transport.connect(active) }
				updateCredentialRotation(
					CredentialRotationPhase.FAILED,
					active.keyVersion + 1,
					"The active credential was restored, but Core's expired pending state could not be cancelled. Tap Resume rotation.",
				)
			}
		}
	}

	private suspend fun recoverRejectedCredentialCandidate(
		active: DeviceCredential,
		candidate: CredentialRotationCandidate,
	) {
		try {
			transport.armCredentialRotation(null)
			transport.cancelCredentialRotation(active)
			credentialStore.clearRotationCandidate()
			transport.connect(active)
			updateCredentialRotation(
				CredentialRotationPhase.FAILED,
				candidate.keyVersion,
				"Core rejected the replacement. The previous credential remains active; try rotating again.",
			)
		} catch (error: CredentialRotationException) {
			if (error.kind == CredentialRotationFailureKind.UNAUTHORIZED) {
				requirePairingAfterRotationFailure("Neither saved credential is authorized. Pair this device again.")
			} else {
				runCatching { transport.connect(active) }
				updateCredentialRotation(
					CredentialRotationPhase.FAILED,
					candidate.keyVersion,
					"The replacement remains stored until Core can verify or cancel it. Tap Resume rotation.",
				)
			}
		}
	}

	private suspend fun recoverCorruptRotationState(active: DeviceCredential) {
		updateCredentialRotation(
			CredentialRotationPhase.RECOVERING,
			active.keyVersion + 1,
			"Repairing incomplete local credential rotation state.",
		)
		transport.armCredentialRotation(null)
		runCatching { transport.cancelCredentialRotation(active) }
		runCatching { credentialStore.clearRotationCandidate() }
		runCatching { transport.connect(active) }
		updateCredentialRotation(
			CredentialRotationPhase.FAILED,
			active.keyVersion + 1,
			"Incomplete replacement state was discarded. The previous credential remains active.",
		)
	}

	private suspend fun promoteCredentialAndConnect(candidate: CredentialRotationCandidate) {
		val promoted = try {
			credentialStore.promoteRotationCandidate(candidate.keyVersion)
		} catch (error: Exception) {
			val alreadyPromoted = runCatching { credentialStore.read() }.getOrNull()
			if (alreadyPromoted?.keyVersion == candidate.keyVersion &&
				alreadyPromoted.deviceId == candidate.deviceId
			) {
				alreadyPromoted
			} else {
				transport.connect(candidate.toPromotedCredential())
				updateCredentialRotation(
					CredentialRotationPhase.FAILED,
					candidate.keyVersion,
					"Core promoted the replacement, but local slot promotion is incomplete. Restart or tap Resume rotation.",
				)
				return
			}
		}
		transport.connect(promoted)
		updateCredentialRotation(
			CredentialRotationPhase.SUCCEEDED,
			promoted.keyVersion,
			"Credential rotation completed. The previous credential was removed.",
		)
		mutableSnapshot.update { it.copy(credentialKeyVersion = promoted.keyVersion) }
	}

	private suspend fun completeCredentialRotationFromSocket(keyVersion: Int) {
		credentialRotationMutex.withLock {
			val candidate = runCatching { credentialStore.readRotationCandidate() }.getOrNull()
			if (candidate != null && candidate.keyVersion == keyVersion) {
				promoteCredentialAndConnect(candidate)
				return@withLock
			}
			val active = runCatching { credentialStore.read() }.getOrNull()
			if (active != null && active.keyVersion == keyVersion) {
				transport.connect(active)
				mutableSnapshot.update {
					it.copy(
						credentialKeyVersion = active.keyVersion,
						credentialRotation = CredentialRotationState(
							phase = CredentialRotationPhase.SUCCEEDED,
							targetKeyVersion = active.keyVersion,
							message = "Credential rotation completed and the promoted stream reconnected.",
						),
					)
				}
				return@withLock
			}
			requirePairingAfterRotationFailure(
				"Core confirmed credential rotation, but the replacement is unavailable. Pair this device again.",
			)
		}
	}

	private suspend fun requirePairingAfterRotationFailure(message: String) {
		runCatching { transport.disconnect() }
		runCatching { credentialStore.clear() }
		mutableSnapshot.update {
			it.copy(
				isPaired = false,
				connection = ConnectionStatus.Offline,
				pairingError = message,
				credentialKeyVersion = 0,
				credentialRotation = CredentialRotationState(
					phase = CredentialRotationPhase.FAILED,
					message = message,
				),
			)
		}
	}

	private fun updateCredentialRotation(
		phase: CredentialRotationPhase,
		targetKeyVersion: Int? = null,
		message: String,
	) {
		mutableSnapshot.update {
			it.copy(
				credentialRotation = CredentialRotationState(
					phase = phase,
					targetKeyVersion = targetKeyVersion,
					message = message,
				),
			)
		}
	}

	private fun rotationFailureMessage(
		error: CredentialRotationException,
		candidateStored: Boolean,
	): String = when (error.kind) {
		CredentialRotationFailureKind.PENDING ->
			"Core still has a pending rotation. Tap Resume rotation to reconcile it safely."
		CredentialRotationFailureKind.INVALID_RESPONSE ->
			"Core returned invalid rotation metadata. No credential was discarded."
		CredentialRotationFailureKind.TRANSIENT -> if (candidateStored) {
			"Confirmation did not finish. The replacement is stored safely; tap Resume rotation."
		} else {
			"Credential rotation could not start. The current credential remains active."
		}
		CredentialRotationFailureKind.EXPIRED ->
			"The replacement expired. The current credential remains active."
		CredentialRotationFailureKind.UNAUTHORIZED ->
			"The saved credential is no longer authorized. Pair this device again."
	}

    private suspend fun applyAuthoritativeSnapshot(event: CoreEvent.AuthoritativeSnapshot) {
			val previousVoice = mutableSnapshot.value.voiceSession
			retentionOutcomeUnknown = false
			val retainTranscript = event.transcriptRetention
			val authoritative = CacheSnapshot(
				messages = if (retainTranscript) event.messages else emptyList(),
				tasks = event.tasks.map { task ->
					if (retainTranscript) task else task.redactedForCache()
				},
			)
			val localFailure = cacheMutex.withLock {
				latestPreferences = latestPreferences.copy(retainTranscript = retainTranscript)
				if (retainTranscript) {
					val preferenceFailure = runCatching {
						preferenceStore.setRetainTranscript(true)
					}.exceptionOrNull()
					val cacheFailure = runCatching {
						cache.replaceAuthoritative(authoritative)
					}.exceptionOrNull()
					preferenceFailure ?: cacheFailure
				} else {
					// Delete/redact content before the fallible DataStore write. The
					// two writes are attempted independently, so either durable layer
					// is sufficient to keep a later bootstrap fail-closed.
					val cacheFailure = runCatching {
						cache.replaceAuthoritative(authoritative)
					}.exceptionOrNull()
					val preferenceFailure = runCatching {
						preferenceStore.setRetainTranscript(false)
					}.exceptionOrNull()
					cacheFailure ?: preferenceFailure
				}
			}
			mutableSnapshot.update { current ->
				current.copy(
					conversationId = event.conversationId,
					messages = authoritative.messages.sortedBy(ConversationMessage::createdAtEpochMillis),
					tasks = authoritative.tasks.associateBy(TaskRecord::id),
					approvals = event.approvals.associateBy(ApprovalRequest::id),
					partialTranscript = "",
					finalTranscript = "",
					voiceSession = event.voiceSession,
					retainTranscript = retainTranscript,
					retentionChangeInProgress = false,
					retentionStateAuthoritative = true,
				)
			}
			if (localFailure != null) {
				publishError(localFailure.safeMessage("Core privacy state is current, but it could not be persisted locally."))
			}

			when {
				event.voiceSession != null -> handleVoiceChanged(event.voiceSession)
				previousVoice != null -> {
					runCatching { speechPlayback.stop() }
					runCatching { mediaTransport.stop() }
					audioRoutes.stop()
					activeMediaSessionId = null
					readyMediaSessionId = null
					calls.endActiveCall()
				}
			}
	}

    private fun handleVoiceChanged(session: VoiceSession) {
		val sessionWithLocalPlayback = if (speechPlayback.handlesPlayback) {
			session.copy(ttsPlayback = latestSpeechPlaybackState.status)
		} else {
			session
		}
        mutableSnapshot.update {
			it.copy(conversationId = session.conversationId, voiceSession = sessionWithLocalPlayback)
        }
        when (session.phase) {
            DialogPhase.RINGING -> calls.publishIncomingCall(session)
            DialogPhase.ENDED -> {
                scope.launch {
                    runCatching { speechPlayback.stop() }
                    runCatching { mediaTransport.stop() }
                    audioRoutes.stop()
                    activeMediaSessionId = null
                    readyMediaSessionId = null
                    calls.endActiveCall()
                    if (!latestPreferences.retainTranscript) {
                        mutableSnapshot.update { it.copy(partialTranscript = "", finalTranscript = "") }
                    }
                }
            }
            else -> {
                if (readyMediaSessionId == session.id) {
                    calls.updateActiveCall(session)
                } else if (activeMediaSessionId != session.id) {
                    activeMediaSessionId = session.id
                    scope.launch {
                        runCatching {
                            mediaTransport.start(MediaSessionConfig(sessionId = session.id))
                            mediaTransport.setMuted(session.isMuted)
                            readyMediaSessionId = session.id
                            if (latestPreferences.preferPushToTalk && !session.isPushToTalk) {
                                setPushToTalk(session.id, enabled = true)
                            }
                            mutableSnapshot.value.voiceSession?.takeIf { it.id == session.id }?.let {
                                calls.updateActiveCall(it)
                            }
                        }.onFailure { error ->
                            activeMediaSessionId = null
                            readyMediaSessionId = null
                            publishError(error.safeMessage("The call media session could not start."))
                        }
                    }
                }
            }
        }
    }

    private fun applyMediaState(mediaState: MediaTransportState) {
        val currentSession = mutableSnapshot.value.voiceSession ?: return
        when (mediaState) {
            MediaTransportState.Idle, MediaTransportState.Ended -> Unit
            is MediaTransportState.Connecting -> {
                mutableSnapshot.update {
                    it.copy(
                        voiceSession = currentSession.copy(
                            isSimulatedMedia = mediaState.isSimulated,
                            mediaNotice = if (mediaState.isSimulated) {
                                "Starting the local media simulator."
                            } else {
                                "Negotiating encrypted WebRTC media."
                            },
                        ),
                    )
                }
            }
            is MediaTransportState.Active -> {
                mutableSnapshot.update {
                    it.copy(
                        voiceSession = currentSession.copy(
                            isSimulatedMedia = mediaState.isSimulated,
                            mediaNotice = mediaState.notice,
                        ),
                    )
                }
            }
            is MediaTransportState.Failed -> publishError(mediaState.safeMessage)
        }
    }

    private fun applySpeechPlaybackState(playbackState: SpeechPlaybackState) {
		latestSpeechPlaybackState = playbackState
        val currentSession = mutableSnapshot.value.voiceSession ?: return
        if (playbackState.sessionId != null && playbackState.sessionId != currentSession.id) return
        mutableSnapshot.update { current ->
            current.copy(
                voiceSession = current.voiceSession?.copy(ttsPlayback = playbackState.status),
                errorMessage = playbackState.safeMessage ?: current.errorMessage,
            )
        }
    }

	private fun requireRetentionOutcomeKnown() {
		check(!retentionOutcomeUnknown && !transport.isAwaitingAuthoritativeSnapshot()) {
			"Core has not confirmed the current privacy preference. Wait for the authoritative reconnect snapshot."
		}
	}

	private fun markRetentionAwaitingSnapshot() {
		retentionOutcomeUnknown = true
		mutableSnapshot.update {
			it.copy(
				retentionChangeInProgress = false,
				retentionStateAuthoritative = false,
			)
		}
	}

    private suspend fun executeCommand(block: suspend () -> Unit) {
        runCatching { block() }
            .onFailure { publishError(it.safeMessage("The requested action could not be completed.")) }
    }

	private suspend fun cacheSafely(block: suspend () -> Unit) {
		runCatching { block() }
			.onFailure { publishError(it.safeMessage("A local cache update failed.")) }
	}

    private fun publishError(message: String) {
        mutableSnapshot.update { it.copy(errorMessage = message) }
    }
}

private fun Throwable.safeMessage(fallback: String): String =
	message?.takeIf { it.isNotBlank() && it.length <= 240 } ?: fallback

private fun TaskRecord.redactedForCache(): TaskRecord = copy(
	goal = "[transcript retention disabled]",
	summary = "",
)

private const val INITIAL_SNAPSHOT_TIMEOUT_MILLIS = 15_000L
