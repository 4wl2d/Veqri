package com.veqri.android.data

import com.veqri.android.FakeAudioRoutes
import com.veqri.android.MemoryConversationCache
import com.veqri.android.MemoryCredentialStore
import com.veqri.android.MemoryPreferenceStore
import com.veqri.android.ManualCoreTransport
import com.veqri.android.RecordingCallController
import com.veqri.android.RecordingSpeechPlayback
import com.veqri.android.media.SimulatedVoiceMediaTransport
import com.veqri.android.network.EpochClock
import com.veqri.android.network.CoreCommand
import com.veqri.android.network.CoreEvent
import com.veqri.android.network.CommandCommitException
import com.veqri.android.network.CommandCommitFailureKind
import com.veqri.android.network.CommandCommitResult
import com.veqri.android.network.CredentialRotationException
import com.veqri.android.network.CredentialRotationFailureKind
import com.veqri.android.network.FakeCoreTransport
import com.veqri.android.network.IdSource
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.async
import kotlinx.coroutines.awaitCancellation
import kotlinx.coroutines.cancelAndJoin
import kotlinx.coroutines.launch
import kotlinx.coroutines.test.runCurrent
import kotlinx.coroutines.test.runTest
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

@OptIn(ExperimentalCoroutinesApi::class)
class VeqriRepositoryTest {
	@Test
	fun `startup confirms durable candidate before opening general stream`() = runTest {
		val active = activeCredential()
		val candidate = rotationCandidate()
		val credentials = MemoryCredentialStore().apply {
			value = active
			rotationCandidate = candidate
		}
		val transport = ManualCoreTransport()
		val repository = repository(transport, credentials)
		runCurrent()

		assertEquals(2, credentials.value?.keyVersion)
		assertEquals(null, credentials.rotationCandidate)
		assertEquals(2, transport.connectedCredential?.keyVersion)
		assertTrue(transport.operations.indexOf("confirm:2") < transport.operations.indexOf("connect:2"))
		assertTrue("connect:1" !in transport.operations)
		assertEquals(CredentialRotationPhase.SUCCEEDED, repository.snapshot.value.credentialRotation.phase)
	}

	@Test
	fun `expired startup candidate restores cancels and rotates with a fresh candidate`() = runTest {
		val credentials = MemoryCredentialStore().apply {
			value = activeCredential()
			rotationCandidate = rotationCandidate()
		}
		val transport = ManualCoreTransport().apply {
			confirmationFailures += CredentialRotationException(
				CredentialRotationFailureKind.EXPIRED,
				"expired",
			)
		}
		val repository = repository(transport, credentials)
		runCurrent()

		val restoreIndex = transport.operations.indexOf("connect:1")
		val freshPrepareIndex = transport.operations.indexOf("prepare:1")
		assertTrue(restoreIndex >= 0 && freshPrepareIndex > restoreIndex)
		assertTrue(transport.operations.count { it == "cancel:1" } >= 1)
		assertEquals(2, credentials.value?.keyVersion)
		assertEquals(null, credentials.rotationCandidate)
		assertEquals(CredentialRotationPhase.SUCCEEDED, repository.snapshot.value.credentialRotation.phase)
	}

	@Test
	fun `socket 4004 commit proof wins when confirmation response is lost`() = runTest {
		val credentials = MemoryCredentialStore().apply { value = activeCredential() }
		val transport = ManualCoreTransport().apply {
			beforeConfirmResult = { candidate ->
				emitCredentialRotationCommitted(candidate.keyVersion, candidate.correlationId)
			}
			confirmationFailures += CredentialRotationException(
				CredentialRotationFailureKind.TRANSIENT,
				"response lost",
			)
		}
		val repository = repository(transport, credentials)
		runCurrent()

		repository.rotateCredential()
		runCurrent()

		assertEquals(2, credentials.value?.keyVersion)
		assertEquals(null, credentials.rotationCandidate)
		assertEquals(2, transport.connectedCredential?.keyVersion)
		assertEquals(CredentialRotationPhase.SUCCEEDED, repository.snapshot.value.credentialRotation.phase)
		assertTrue(repository.snapshot.value.credentialRotation.message.contains("token").not())
	}

	@Test
	fun `late socket 4004 proof is idempotent after HTTP confirmation`() = runTest {
		val credentials = MemoryCredentialStore().apply { value = activeCredential() }
		val transport = ManualCoreTransport()
		val repository = repository(transport, credentials)
		runCurrent()

		repository.rotateCredential()
		transport.emitCredentialRotationCommitted(keyVersion = 2)
		runCurrent()

		assertEquals(2, credentials.value?.keyVersion)
		assertEquals(null, credentials.rotationCandidate)
		assertEquals(2, transport.connectedCredential?.keyVersion)
		assertEquals(CredentialRotationPhase.SUCCEEDED, repository.snapshot.value.credentialRotation.phase)
	}

	@Test
	fun `neither credential authorized stops retries and requires pairing without exposing tokens`() = runTest {
		val credentials = MemoryCredentialStore().apply {
			value = activeCredential()
			rotationCandidate = rotationCandidate()
		}
		val unauthorized = CredentialRotationException(
			CredentialRotationFailureKind.UNAUTHORIZED,
			"unauthorized",
		)
		val transport = ManualCoreTransport().apply {
			confirmationFailures += unauthorized
			cancelFailure = unauthorized
		}
		val repository = repository(transport, credentials)
		runCurrent()

		assertTrue(repository.snapshot.value.isPaired.not())
		assertEquals(null, credentials.value)
		assertEquals(null, credentials.rotationCandidate)
		val message = requireNotNull(repository.snapshot.value.pairingError)
		assertTrue(message.contains("Pair"))
		assertTrue(message.contains("active-token").not())
		assertTrue(message.contains("candidate-token").not())
		assertEquals("disconnect", transport.operations.last())
	}
	@Test
	fun `authoritative snapshot prunes stale offline state and replaces cache atomically`() = runTest {
		val transport = ManualCoreTransport()
		val cache = MemoryConversationCache().apply {
			messages["stale-message"] = message("stale-message", "old conversation", 1)
			tasks["stale-task"] = task("stale-task", TaskStatus.RUNNING, canRetry = false)
		}
		val routes = FakeAudioRoutes()
		val calls = RecordingCallController()
		val repository = VeqriRepository(
			scope = backgroundScope,
			transport = transport,
			credentialStore = MemoryCredentialStore(),
			preferenceStore = MemoryPreferenceStore(),
			cache = cache,
			mediaTransport = SimulatedVoiceMediaTransport(routes),
			audioRoutes = routes,
			calls = calls,
		)
		runCurrent()
		transport.emit(
			CoreEvent.ApprovalChanged(
				eventId = "stale-approval-event",
				correlationId = "stale",
				approval = ApprovalRequest(
					id = "stale-approval",
					taskId = "stale-task",
					title = "Stale",
					redactedArguments = "hidden",
					risk = ApprovalRisk.STATE_CHANGING,
					expiresAtEpochMillis = 10,
					status = ApprovalStatus.PENDING,
				),
			),
		)
		runCurrent()

		val freshMessage = message("fresh-message", "fresh conversation", 20)
		val freshTask = task("fresh-task", TaskStatus.FAILED, canRetry = true)
		transport.emit(
			CoreEvent.AuthoritativeSnapshot(
				eventId = "snapshot-event",
				correlationId = "snapshot",
				snapshotId = "snapshot-1",
				conversationId = "conversation-1",
				transcriptRetention = true,
				messages = listOf(freshMessage),
				tasks = listOf(freshTask),
				approvals = emptyList(),
				voiceSession = null,
			),
		)
		runCurrent()

		assertEquals(listOf("fresh-message"), repository.snapshot.value.messages.map { it.id })
		assertEquals(setOf("fresh-task"), repository.snapshot.value.tasks.keys)
		assertTrue(repository.snapshot.value.approvals.isEmpty())
		assertEquals(setOf("fresh-message"), cache.messages.keys)
		assertEquals(setOf("fresh-task"), cache.tasks.keys)
		assertTrue(calls.completed.isEmpty())
		assertEquals(1, calls.approvals.size)

		transport.emit(
			CoreEvent.TaskChanged(
				eventId = "dismiss-event",
				correlationId = "dismiss",
				task = freshTask.copy(dismissed = true, canRetry = false),
			),
		)
		runCurrent()
		assertTrue("fresh-task" !in repository.snapshot.value.tasks)
		assertTrue("fresh-task" !in cache.tasks)
	}

	@Test
	fun `retry command requires Core eligibility metadata`() = runTest {
		val transport = ManualCoreTransport()
		val routes = FakeAudioRoutes()
		val repository = VeqriRepository(
			scope = backgroundScope,
			transport = transport,
			credentialStore = MemoryCredentialStore(),
			preferenceStore = MemoryPreferenceStore(),
			cache = MemoryConversationCache(),
			mediaTransport = SimulatedVoiceMediaTransport(routes),
			audioRoutes = routes,
			calls = RecordingCallController(),
			idSource = IdSource { "retry-command" },
		)
		runCurrent()
		transport.emit(
			CoreEvent.AuthoritativeSnapshot(
				eventId = "snapshot-event",
				correlationId = "snapshot",
				snapshotId = "snapshot-1",
				conversationId = null,
				transcriptRetention = true,
				messages = emptyList(),
				tasks = listOf(
					task("shell-task", TaskStatus.FAILED, canRetry = false),
					task("eligible-task", TaskStatus.FAILED, canRetry = true),
				),
				approvals = emptyList(),
				voiceSession = null,
			),
		)
		runCurrent()

		repository.retryTask("shell-task")
		repository.retryTask("eligible-task")

		val retries = transport.sentCommands.filterIsInstance<CoreCommand.RetryTask>()
		assertEquals(listOf("eligible-task"), retries.map { it.taskId })
	}

    @Test
    fun `one logical tts event speaks once and barge in stops local playback`() = runTest {
        val transport = ManualCoreTransport()
        val routes = FakeAudioRoutes()
        val speech = RecordingSpeechPlayback()
        val repository = VeqriRepository(
            scope = backgroundScope,
            transport = transport,
            credentialStore = MemoryCredentialStore(),
            preferenceStore = MemoryPreferenceStore(),
            cache = MemoryConversationCache(),
            mediaTransport = SimulatedVoiceMediaTransport(routes),
            audioRoutes = routes,
            calls = RecordingCallController(),
            speechPlayback = speech,
            idSource = IdSource { "interrupt-command" },
        )
        runCurrent()
        val session = VoiceSession(
            id = "session-1",
            conversationId = "conversation-1",
            direction = CallDirection.OUTGOING,
            phase = DialogPhase.LISTENING,
            startedAtEpochMillis = 1,
            isMuted = false,
            isPushToTalk = false,
            selectedAudioRoute = AudioRouteKind.EARPIECE,
            ttsPlayback = TtsPlaybackStatus.IDLE,
            isSimulatedMedia = true,
            mediaNotice = "test",
        )
        transport.emit(CoreEvent.VoiceChanged("voice-event", "correlation", session))
        transport.emit(
            CoreEvent.TtsSpeak(
                "speak-event",
                "correlation",
                session.id,
                session.conversationId,
                "The full response is spoken exactly once.",
            ),
        )
        transport.emit(CoreEvent.TtsChanged("chunk-event", "correlation", TtsPlaybackStatus.SPEAKING))
        runCurrent()

        assertEquals(listOf(session.id to "The full response is spoken exactly once."), speech.spoken)
        repository.interruptTts(session.id)
        runCurrent()

        assertEquals(1, speech.stopCount)
        assertEquals(TtsPlaybackStatus.INTERRUPTED, repository.snapshot.value.voiceSession?.ttsPlayback)
        assertEquals(1, transport.sentCommands.filterIsInstance<CoreCommand.InterruptTts>().size)
    }

    @Test
    fun `pair send and stream produce cached immutable snapshot`() = runTest {
        var nextId = 0
        val transport = FakeCoreTransport(
            idSource = IdSource { "id-${nextId++}" },
            clock = EpochClock { 1_700_000_000_000L + nextId },
        )
        val cache = MemoryConversationCache()
        val routes = FakeAudioRoutes()
        val calls = RecordingCallController()
        val repository = VeqriRepository(
            scope = backgroundScope,
            transport = transport,
            credentialStore = MemoryCredentialStore(),
            preferenceStore = MemoryPreferenceStore(),
            cache = cache,
            mediaTransport = SimulatedVoiceMediaTransport(routes),
            audioRoutes = routes,
            calls = calls,
            idSource = IdSource { "command-${nextId++}" },
        )
        runCurrent()

        repository.pair("http://10.0.2.2:8080", "123456", "Test phone")
        repository.sendText("Inspect the repository")
        runCurrent()

        assertTrue(repository.snapshot.value.isPaired)
        assertTrue(repository.snapshot.value.connection is ConnectionStatus.Connected)
        assertEquals(2, repository.snapshot.value.messages.size)
        assertEquals(TaskStatus.COMPLETED, repository.snapshot.value.tasks.values.single().status)
        assertEquals(2, cache.messages.size)
        assertEquals(1, cache.tasks.size)
        assertEquals(1, calls.completed.size)
    }

    @Test
	fun `push to talk mutes until the user is pressing`() = runTest {
        var nextId = 0
        val transport = FakeCoreTransport(
            idSource = IdSource { "id-${nextId++}" },
            clock = EpochClock { 1_700_000_000_000L + nextId },
        )
        val routes = FakeAudioRoutes()
        val media = SimulatedVoiceMediaTransport(routes)
        val repository = VeqriRepository(
            scope = backgroundScope,
            transport = transport,
            credentialStore = MemoryCredentialStore(),
            preferenceStore = MemoryPreferenceStore(),
            cache = MemoryConversationCache(),
            mediaTransport = media,
            audioRoutes = routes,
            calls = RecordingCallController(),
            idSource = IdSource { "command-${nextId++}" },
        )
        runCurrent()
        repository.pair("http://10.0.2.2:8080", "123456", "Test phone")
        repository.startCall()
        runCurrent()
        val sessionId = requireNotNull(repository.snapshot.value.voiceSession).id

        repository.setPushToTalk(sessionId, enabled = true)
        runCurrent()
        assertTrue(repository.snapshot.value.voiceSession?.isMuted == true)
        assertTrue(media.isMutedForTest())

        repository.setPushToTalkPressed(sessionId, pressed = true)
        runCurrent()
        assertTrue(repository.snapshot.value.voiceSession?.isMuted == false)

        repository.setPushToTalkPressed(sessionId, pressed = false)
        runCurrent()
        assertTrue(repository.snapshot.value.voiceSession?.isMuted == true)
	}

	@Test
	fun `disabling retention removes and stops persistent transcript caching`() = runTest {
		var nextId = 0
		val transport = FakeCoreTransport(
			idSource = IdSource { "id-${nextId++}" },
			clock = EpochClock { 1_700_000_000_000L + nextId },
		)
		val cache = MemoryConversationCache()
		val routes = FakeAudioRoutes()
		val repository = VeqriRepository(
			scope = backgroundScope,
			transport = transport,
			credentialStore = MemoryCredentialStore(),
			preferenceStore = MemoryPreferenceStore(),
			cache = cache,
			mediaTransport = SimulatedVoiceMediaTransport(routes),
			audioRoutes = routes,
			calls = RecordingCallController(),
			idSource = IdSource { "command-${nextId++}" },
		)
		runCurrent()
		repository.pair("http://10.0.2.2:8080", "123456", "Privacy phone")
		repository.sendText("First retained message")
		runCurrent()
		assertTrue(cache.messages.isNotEmpty())

		repository.setRetainTranscript(false)
		runCurrent()
		assertTrue(cache.messages.isEmpty())
		assertTrue(cache.tasks.values.all { it.goal == "[transcript retention disabled]" })

		repository.sendText("Visible live but not cached")
		runCurrent()
		assertTrue(repository.snapshot.value.messages.isNotEmpty())
		assertTrue(cache.messages.isEmpty())
		assertTrue(cache.tasks.values.all { it.goal == "[transcript retention disabled]" })
	}

	@Test
	fun `retention waits for Core commit and serializes an immediate send`() = runTest {
		val transport = ManualCoreTransport()
		val preferences = MemoryPreferenceStore()
		val commitGate = CompletableDeferred<Unit>()
		transport.commandCommit = { command ->
			commitGate.await()
			CommandCommitResult(command.commandId, "conversation.set_transcript_retention")
		}
		val routes = FakeAudioRoutes()
		val repository = VeqriRepository(
			scope = backgroundScope,
			transport = transport,
			credentialStore = MemoryCredentialStore(),
			preferenceStore = preferences,
			cache = MemoryConversationCache(),
			mediaTransport = SimulatedVoiceMediaTransport(routes),
			audioRoutes = routes,
			calls = RecordingCallController(),
			idSource = IdSource { "privacy-${transport.sentCommands.size}" },
		)
		runCurrent()

		val retention = async { repository.setRetainTranscript(false) }
		runCurrent()
		val send = async { repository.sendText("Send only after privacy commit") }
		runCurrent()

		assertTrue(repository.snapshot.value.retainTranscript)
		assertTrue(repository.snapshot.value.retentionChangeInProgress)
		assertTrue(preferences.currentValue.retainTranscript)
		assertEquals(1, transport.sentCommands.size)

		commitGate.complete(Unit)
		retention.await()
		send.await()
		val sentText = transport.sentCommands.filterIsInstance<CoreCommand.SendText>().single()
		assertTrue(!sentText.retainTranscript)
		assertTrue(!preferences.currentValue.retainTranscript)
		assertTrue(!repository.snapshot.value.retainTranscript)
	}

	@Test
	fun `confirmed opt out scrubs before preference failure and restart never exposes paired cache`() = runTest {
		val transport = ManualCoreTransport()
		val preferences = MemoryPreferenceStore()
		val credentials = MemoryCredentialStore().apply { value = activeCredential() }
		val cache = MemoryConversationCache()
		val routes = FakeAudioRoutes()
		val repository = VeqriRepository(
			scope = backgroundScope,
			transport = transport,
			credentialStore = credentials,
			preferenceStore = preferences,
			cache = cache,
			mediaTransport = SimulatedVoiceMediaTransport(routes),
			audioRoutes = routes,
			calls = RecordingCallController(),
			idSource = IdSource { "privacy-storage-failure" },
		)
		runCurrent()
		val retainedMessage = message("retained-before-opt-out", "private transcript", 1)
		val retainedTask = task("retained-task-before-opt-out", TaskStatus.COMPLETED, canRetry = false)
		transport.emit(
			CoreEvent.AuthoritativeSnapshot(
				eventId = "retained-snapshot-event",
				correlationId = "retained-snapshot",
				snapshotId = "retained-snapshot",
				conversationId = "conversation-1",
				transcriptRetention = true,
				messages = listOf(retainedMessage),
				tasks = listOf(retainedTask),
				approvals = emptyList(),
				voiceSession = null,
			),
		)
		runCurrent()
		assertEquals("private transcript", cache.messages.getValue(retainedMessage.id).text)

		preferences.retainTranscriptWriteFailure = IllegalStateException("DataStore unavailable")
		repository.setRetainTranscript(false)
		runCurrent()

		assertTrue(preferences.currentValue.retainTranscript)
		assertTrue(cache.messages.isEmpty())
		assertEquals("[transcript retention disabled]", cache.tasks.getValue(retainedTask.id).goal)
		assertTrue(repository.snapshot.value.messages.isEmpty())
		assertTrue(!repository.snapshot.value.retainTranscript)

		// Model a process restart from a pre-fix build that left both an old
		// `true` preference and private Room rows behind. Paired bootstrap must
		// sanitize those rows before any offline snapshot can expose them.
		val legacyMessage = message("legacy-restart-message", "must never display", 2)
		val legacyTask = task("legacy-restart-task", TaskStatus.FAILED, canRetry = false)
		cache.messages[legacyMessage.id] = legacyMessage
		cache.tasks[legacyTask.id] = legacyTask
		val offlineTransport = ManualCoreTransport().apply {
			connectFailure = IllegalStateException("Core is offline")
		}
		val restartedRoutes = FakeAudioRoutes()
		val restarted = VeqriRepository(
			scope = backgroundScope,
			transport = offlineTransport,
			credentialStore = credentials,
			preferenceStore = preferences,
			cache = cache,
			mediaTransport = SimulatedVoiceMediaTransport(restartedRoutes),
			audioRoutes = restartedRoutes,
			calls = RecordingCallController(),
		)
		runCurrent()

		assertTrue(restarted.snapshot.value.connection is ConnectionStatus.Offline)
		assertTrue(!restarted.snapshot.value.retentionStateAuthoritative)
		assertTrue(restarted.snapshot.value.messages.isEmpty())
		assertEquals(
			"[transcript retention disabled]",
			restarted.snapshot.value.tasks.getValue(legacyTask.id).goal,
		)
		assertTrue(cache.messages.isEmpty())
		assertEquals("[transcript retention disabled]", cache.tasks.getValue(legacyTask.id).goal)
	}

	@Test
	fun `authoritative opt out scrubs cache even when preference persistence fails`() = runTest {
		val transport = ManualCoreTransport()
		val preferences = MemoryPreferenceStore().apply {
			retainTranscriptWriteFailure = IllegalStateException("DataStore unavailable")
		}
		val cache = MemoryConversationCache().apply {
			messages["private-message"] = message("private-message", "private transcript", 1)
			tasks["private-task"] = task("private-task", TaskStatus.COMPLETED, canRetry = false)
		}
		val routes = FakeAudioRoutes()
		val repository = VeqriRepository(
			scope = backgroundScope,
			transport = transport,
			credentialStore = MemoryCredentialStore(),
			preferenceStore = preferences,
			cache = cache,
			mediaTransport = SimulatedVoiceMediaTransport(routes),
			audioRoutes = routes,
			calls = RecordingCallController(),
		)
		runCurrent()

		transport.emit(
			CoreEvent.AuthoritativeSnapshot(
				eventId = "private-snapshot-event",
				correlationId = "private-snapshot",
				snapshotId = "private-snapshot",
				conversationId = "conversation-1",
				transcriptRetention = false,
				messages = listOf(message("server-private", "must be discarded", 2)),
				tasks = listOf(task("server-private-task", TaskStatus.COMPLETED, canRetry = false)),
				approvals = emptyList(),
				voiceSession = null,
			),
		)
		runCurrent()

		assertTrue(cache.messages.isEmpty())
		assertEquals("[transcript retention disabled]", cache.tasks.getValue("server-private-task").goal)
		assertTrue(repository.snapshot.value.messages.isEmpty())
		assertTrue(!repository.snapshot.value.retainTranscript)
		assertTrue(preferences.currentValue.retainTranscript)
	}

	@Test
	fun `rejected retention command leaves local preference unchanged`() = runTest {
		val transport = ManualCoreTransport().apply {
			commandCommit = {
				throw CommandCommitException(CommandCommitFailureKind.REJECTED, "Core rejected the privacy change.")
			}
		}
		val preferences = MemoryPreferenceStore()
		val repository = repository(transport, MemoryCredentialStore(), preferences)
		runCurrent()

		repository.setRetainTranscript(false)

		assertTrue(preferences.currentValue.retainTranscript)
		assertTrue(repository.snapshot.value.retainTranscript)
		assertTrue(repository.snapshot.value.retentionStateAuthoritative)
		assertTrue(!repository.snapshot.value.retentionChangeInProgress)
	}

	@Test
	fun `unknown retention outcome blocks sends until authoritative reconnect snapshot`() = runTest {
		val transport = ManualCoreTransport().apply {
			commandCommit = {
				throw CommandCommitException(
					CommandCommitFailureKind.OUTCOME_UNKNOWN,
					"Confirmation was lost.",
				)
			}
		}
		val preferences = MemoryPreferenceStore()
		val repository = repository(transport, MemoryCredentialStore(), preferences)
		runCurrent()

		repository.setRetainTranscript(false)
		repository.sendText("Must not race an unknown privacy outcome")
		assertTrue(transport.sentCommands.none { it is CoreCommand.SendText })
		assertTrue(!repository.snapshot.value.retentionStateAuthoritative)
		assertTrue(preferences.currentValue.retainTranscript)

		transport.emit(
			CoreEvent.AuthoritativeSnapshot(
				eventId = "reconnect-snapshot-event",
				correlationId = "reconnect-snapshot",
				snapshotId = "reconnect-snapshot",
				conversationId = "conversation-1",
				transcriptRetention = false,
				messages = listOf(message("private", "must be dropped", 1)),
				tasks = listOf(task("private-task", TaskStatus.COMPLETED, canRetry = false)),
				approvals = emptyList(),
				voiceSession = null,
			),
		)
		runCurrent()

		assertTrue(repository.snapshot.value.retentionStateAuthoritative)
		assertTrue(!repository.snapshot.value.retainTranscript)
		assertTrue(repository.snapshot.value.messages.isEmpty())
		assertEquals("[transcript retention disabled]", repository.snapshot.value.tasks.getValue("private-task").goal)
		assertTrue(!preferences.currentValue.retainTranscript)

		repository.sendText("Safe after reconciliation")
		val sentText = transport.sentCommands.filterIsInstance<CoreCommand.SendText>().single()
		assertTrue(!sentText.retainTranscript)
	}

	@Test
	fun `reconnect blocks stale content commands before its first snapshot`() = runTest {
		val transport = ManualCoreTransport()
		val preferences = MemoryPreferenceStore()
		val repository = repository(transport, MemoryCredentialStore(), preferences)
		runCurrent()
		transport.emit(
			CoreEvent.AuthoritativeSnapshot(
				eventId = "initial-snapshot-event",
				correlationId = "initial-snapshot",
				snapshotId = "initial-snapshot",
				conversationId = "conversation-1",
				transcriptRetention = true,
				messages = emptyList(),
				tasks = emptyList(),
				approvals = emptyList(),
				voiceSession = null,
			),
		)
		runCurrent()

		transport.setConnectionForTest(ConnectionStatus.Reconnecting(1, 0))
		runCurrent()
		transport.setConnectionForTest(ConnectionStatus.Connected(1))
		runCurrent()
		repository.sendText("Must wait for reconnect snapshot")
		assertTrue(transport.sentCommands.none { it is CoreCommand.SendText })
		assertTrue(!repository.snapshot.value.retentionStateAuthoritative)

		transport.emit(
			CoreEvent.AuthoritativeSnapshot(
				eventId = "reconnect-snapshot-event",
				correlationId = "reconnect-snapshot",
				snapshotId = "reconnect-snapshot",
				conversationId = "conversation-1",
				transcriptRetention = false,
				messages = emptyList(),
				tasks = emptyList(),
				approvals = emptyList(),
				voiceSession = null,
			),
		)
		runCurrent()
		repository.sendText("Safe after reconnect snapshot")
		val sent = transport.sentCommands.filterIsInstance<CoreCommand.SendText>().single()
		assertTrue(!sent.retainTranscript)
	}

	@Test
	fun `cancelling an in flight retention waiter requires snapshot reconciliation`() = runTest {
		val transport = ManualCoreTransport().apply {
			commandCommit = { awaitCancellation() }
		}
		val repository = repository(transport, MemoryCredentialStore())
		runCurrent()
		val change = launch { repository.setRetainTranscript(false) }
		runCurrent()
		change.cancelAndJoin()

		assertTrue(!repository.snapshot.value.retentionStateAuthoritative)
		repository.sendText("Must remain blocked after waiter cancellation")
		assertTrue(transport.sentCommands.none { it is CoreCommand.SendText })

		transport.emit(
			CoreEvent.AuthoritativeSnapshot(
				eventId = "post-cancel-snapshot-event",
				correlationId = "post-cancel-snapshot",
				snapshotId = "post-cancel-snapshot",
				conversationId = null,
				transcriptRetention = false,
				messages = emptyList(), tasks = emptyList(), approvals = emptyList(), voiceSession = null,
			),
		)
		runCurrent()
		assertTrue(repository.snapshot.value.retentionStateAuthoritative)
		assertTrue(!repository.snapshot.value.retainTranscript)
	}

	private fun message(id: String, text: String, createdAt: Long) = ConversationMessage(
		id = id,
		conversationId = "conversation-1",
		author = MessageAuthor.ASSISTANT,
		text = text,
		createdAtEpochMillis = createdAt,
		correlationId = "correlation-$id",
	)

	private fun task(id: String, status: TaskStatus, canRetry: Boolean) = TaskRecord(
		id = id,
		rootTaskId = id,
		conversationId = "conversation-1",
		goal = id,
		assignedAgent = "test-agent",
		status = status,
		progressPercent = if (status == TaskStatus.COMPLETED) 100 else 50,
		summary = id,
		createdAtEpochMillis = 1,
		updatedAtEpochMillis = 2,
		correlationId = "correlation-$id",
		canRetry = canRetry,
	)

	private fun activeCredential() = DeviceCredential(
		deviceId = "device-1",
		accessToken = "active-token",
		coreBaseUrl = "http://10.0.2.2:8080",
		issuedAtEpochMillis = 1_699_999_000_000,
		keyVersion = 1,
	)

	private fun rotationCandidate() = CredentialRotationCandidate(
		deviceId = "device-1",
		accessToken = "candidate-token",
		coreBaseUrl = "http://10.0.2.2:8080",
		keyVersion = 2,
		preparedAtEpochMillis = 1_700_000_000_000,
		expiresAtEpochMillis = 1_700_000_300_000,
		correlationId = "rotation-correlation",
	)

	private fun kotlinx.coroutines.test.TestScope.repository(
		transport: ManualCoreTransport,
		credentials: MemoryCredentialStore,
		preferences: MemoryPreferenceStore = MemoryPreferenceStore(),
	): VeqriRepository {
		val routes = FakeAudioRoutes()
		return VeqriRepository(
			scope = backgroundScope,
			transport = transport,
			credentialStore = credentials,
			preferenceStore = preferences,
			cache = MemoryConversationCache(),
			mediaTransport = SimulatedVoiceMediaTransport(routes),
			audioRoutes = routes,
			calls = RecordingCallController(),
		)
	}
}
