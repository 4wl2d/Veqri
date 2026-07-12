package com.veqri.android.network

import com.veqri.android.data.ApprovalRequest
import com.veqri.android.data.ApprovalRisk
import com.veqri.android.data.ApprovalStatus
import com.veqri.android.data.AudioRouteKind
import com.veqri.android.data.CallDirection
import com.veqri.android.data.ConnectionStatus
import com.veqri.android.data.ConversationMessage
import com.veqri.android.data.CredentialRotationCandidate
import com.veqri.android.data.DeviceCredential
import com.veqri.android.data.DialogPhase
import com.veqri.android.data.MessageAuthor
import com.veqri.android.data.TaskRecord
import com.veqri.android.data.TaskStatus
import com.veqri.android.data.TtsPlaybackStatus
import com.veqri.android.data.VoiceSession
import java.util.UUID
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.asSharedFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock

class FakeCoreTransport(
    private val idSource: IdSource = IdSource { UUID.randomUUID().toString() },
    private val clock: EpochClock = EpochClock(System::currentTimeMillis),
    private val stepDelay: FakeStepDelay = FakeStepDelay { },
) : CoreTransport {
    private val mutex = Mutex()
    private val mutableConnection = MutableStateFlow<ConnectionStatus>(ConnectionStatus.Offline)
    private val mutableEvents = MutableSharedFlow<CoreEvent>(extraBufferCapacity = 64)
    private var activeCredential: DeviceCredential? = null
    private var pendingRotation: CredentialRotationCandidate? = null
    private var armedRotation: CredentialRotationCandidate? = null
    private var conversationId: String? = null
    private var voiceSession: VoiceSession? = null
    private var transcriptRetention = true
	@Volatile
	private var awaitingAuthoritativeSnapshot = false
    private val tasks = linkedMapOf<String, TaskRecord>()
    private val approvals = linkedMapOf<String, ApprovalRequest>()
    private val usedPairingCodes = mutableSetOf<String>()

    override val connection = mutableConnection.asStateFlow()
    override val events: Flow<CoreEvent> = mutableEvents.asSharedFlow()

    override suspend fun pair(request: PairingRequest): Result<DeviceCredential> = mutex.withLock {
        when {
            request.oneTimeCode != DEVELOPMENT_PAIRING_CODE -> {
                Result.failure(PairingException("The one-time pairing code is invalid or expired."))
            }
            request.clientProtocolVersion != CoreTransport.PROTOCOL_VERSION -> {
                Result.failure(PairingException("The core and Android protocol versions do not match."))
            }
            request.oneTimeCode in usedPairingCodes -> {
                Result.failure(PairingException("The one-time pairing code has already been used."))
            }
            else -> {
                usedPairingCodes += request.oneTimeCode
				transcriptRetention = request.retainTranscript
                Result.success(
                    DeviceCredential(
                        deviceId = "android-${idSource.nextId()}",
                        accessToken = "local-simulator-token-${idSource.nextId()}",
                        coreBaseUrl = request.coreBaseUrl,
                        issuedAtEpochMillis = clock.nowMillis(),
                    ),
                )
            }
        }
    }

    override suspend fun prepareCredentialRotation(active: DeviceCredential): CredentialRotationCandidate =
        mutex.withLock {
            check(activeCredential == active) { "The active simulator credential changed." }
            if (pendingRotation != null) {
                throw CredentialRotationException(
                    CredentialRotationFailureKind.PENDING,
                    "The simulator already has a pending credential rotation.",
                )
            }
            CredentialRotationCandidate(
                deviceId = active.deviceId,
                accessToken = "local-simulator-rotation-${idSource.nextId()}",
                coreBaseUrl = active.coreBaseUrl,
                keyVersion = active.keyVersion + 1,
                preparedAtEpochMillis = clock.nowMillis(),
                expiresAtEpochMillis = clock.nowMillis() + ROTATION_TTL_MILLIS,
                correlationId = idSource.nextId(),
            ).also { pendingRotation = it }
        }

    override suspend fun armCredentialRotation(candidate: CredentialRotationCandidate?) {
        mutex.withLock { armedRotation = candidate }
    }

    override suspend fun confirmCredentialRotation(
        candidate: CredentialRotationCandidate,
    ): CredentialRotationConfirmation = mutex.withLock {
        if (candidate.expiresAtEpochMillis <= clock.nowMillis()) {
            throw CredentialRotationException(
                CredentialRotationFailureKind.EXPIRED,
                "The simulator replacement credential expired.",
            )
        }
        val pending = pendingRotation
        if (pending == null || pending.accessToken != candidate.accessToken ||
            pending.keyVersion != candidate.keyVersion
        ) {
            val promoted = activeCredential
            if (promoted?.accessToken == candidate.accessToken && promoted.keyVersion == candidate.keyVersion) {
                return@withLock CredentialRotationConfirmation(
                    deviceId = candidate.deviceId,
                    keyVersion = candidate.keyVersion,
                    alreadyConfirmed = true,
                    correlationId = candidate.correlationId,
                )
            }
            throw CredentialRotationException(
                CredentialRotationFailureKind.UNAUTHORIZED,
                "The simulator replacement credential was not accepted.",
            )
        }
        activeCredential = candidate.toPromotedCredential()
        pendingRotation = null
        armedRotation = null
        CredentialRotationConfirmation(
            deviceId = candidate.deviceId,
            keyVersion = candidate.keyVersion,
            alreadyConfirmed = false,
            correlationId = candidate.correlationId,
        )
    }

    override suspend fun cancelCredentialRotation(active: DeviceCredential): Boolean = mutex.withLock {
        if (activeCredential != active) {
            throw CredentialRotationException(
                CredentialRotationFailureKind.UNAUTHORIZED,
                "The active simulator credential changed.",
            )
        }
        val cancelled = pendingRotation != null
        pendingRotation = null
        armedRotation = null
        cancelled
    }

    override suspend fun connect(credential: DeviceCredential) {
		awaitingAuthoritativeSnapshot = true
		val snapshot = mutex.withLock {
            mutableConnection.value = ConnectionStatus.Connecting
            activeCredential = credential
			authoritativeSnapshot()
        }
		mutableEvents.emit(snapshot)
		awaitingAuthoritativeSnapshot = false
		mutableConnection.value = ConnectionStatus.Connected(CoreTransport.PROTOCOL_VERSION)
    }

    override suspend fun send(command: CoreCommand) {
        mutex.withLock {
            check(activeCredential != null) { "Pair and connect before sending commands." }
            when (command) {
                is CoreCommand.SendText -> handleText(command)
				is CoreCommand.SetTranscriptRetention -> transcriptRetention = command.enabled
                is CoreCommand.StartCall -> startCall(CallDirection.OUTGOING, command.commandId)
                is CoreCommand.SimulateIncomingCall -> startCall(CallDirection.INCOMING, command.commandId)
                is CoreCommand.AnswerCall -> updateVoice(command.commandId) {
                    it.copy(phase = DialogPhase.LISTENING, startedAtEpochMillis = clock.nowMillis())
                }
                is CoreCommand.DeclineCall -> updateVoice(command.commandId) {
                    it.copy(phase = DialogPhase.ENDED)
                }
                is CoreCommand.EndCall -> updateVoice(command.commandId) {
                    it.copy(phase = DialogPhase.ENDED, ttsPlayback = TtsPlaybackStatus.IDLE)
                }
                is CoreCommand.SetMuted -> updateVoice(command.commandId) {
                    it.copy(isMuted = command.isMuted)
                }
                is CoreCommand.SetPushToTalk -> updateVoice(command.commandId) {
                    it.copy(isPushToTalk = command.enabled)
                }
                is CoreCommand.SelectAudioRoute -> updateVoice(command.commandId) {
                    it.copy(selectedAudioRoute = command.route)
                }
                is CoreCommand.InterruptTts -> interruptTts(command)
                is CoreCommand.ResolveApproval -> resolveApproval(command)
                is CoreCommand.CancelTask -> cancelTask(command)
                is CoreCommand.RetryTask -> retryTask(command)
            }
        }
    }

    override suspend fun sendAwaitingCommit(command: CoreCommand): CommandCommitResult {
        send(command)
        return CommandCommitResult(command.commandId, commandType(command))
    }

    override suspend fun disconnect() {
        mutex.withLock {
            activeCredential = null
            armedRotation = null
            mutableConnection.value = ConnectionStatus.Offline
        }
    }

	override fun isAwaitingAuthoritativeSnapshot(): Boolean = awaitingAuthoritativeSnapshot

    suspend fun simulateNetworkLoss() {
		awaitingAuthoritativeSnapshot = true
		val snapshot = mutex.withLock {
            check(activeCredential != null) { "Connect before simulating a network loss." }
            val existingConversationId = conversationId
            mutableConnection.value = ConnectionStatus.Reconnecting(attempt = 1, retryInMillis = 250)
            stepDelay.await(FakeStep.RECONNECTED)
            conversationId = existingConversationId
			authoritativeSnapshot()
        }
		mutableEvents.emit(snapshot)
		awaitingAuthoritativeSnapshot = false
		mutableConnection.value = ConnectionStatus.Connected(CoreTransport.PROTOCOL_VERSION)
    }

	private fun authoritativeSnapshot() = CoreEvent.AuthoritativeSnapshot(
		eventId = "snapshot:${idSource.nextId()}",
		correlationId = idSource.nextId(),
		snapshotId = idSource.nextId(),
		conversationId = conversationId,
		transcriptRetention = transcriptRetention,
		messages = emptyList(),
		tasks = tasks.values.toList(),
		approvals = approvals.values.toList(),
		voiceSession = voiceSession,
	)

    private suspend fun handleText(command: CoreCommand.SendText) {
        val activeConversationId = command.conversationId ?: conversationId ?: idSource.nextId()
        conversationId = activeConversationId
        val correlationId = command.commandId
        val now = clock.nowMillis()
        emit(
            CoreEvent.MessageAdded(
                eventId = idSource.nextId(),
                correlationId = correlationId,
                message = ConversationMessage(
                    id = idSource.nextId(),
                    conversationId = activeConversationId,
                    author = MessageAuthor.USER,
                    text = command.text,
                    createdAtEpochMillis = now,
                    correlationId = correlationId,
                ),
            ),
        )
        stepDelay.await(FakeStep.ACCEPTED)

        val taskId = idSource.nextId()
        var task = TaskRecord(
            id = taskId,
            rootTaskId = taskId,
            conversationId = activeConversationId,
            goal = command.text,
            assignedAgent = "coding",
            status = TaskStatus.QUEUED,
            progressPercent = 5,
            summary = "Accepted by the deterministic local agent simulator.",
            createdAtEpochMillis = now,
            updatedAtEpochMillis = now,
            correlationId = correlationId,
        )
        tasks[taskId] = task
        emit(task.asEvent(correlationId))
        stepDelay.await(FakeStep.DELEGATED)

        task = task.copy(
            status = TaskStatus.RUNNING,
            progressPercent = 55,
            summary = "Inspecting the local request and assembling a result.",
            updatedAtEpochMillis = clock.nowMillis(),
        )
        tasks[taskId] = task
        emit(task.asEvent(correlationId))
        if (command.text.contains("approval", ignoreCase = true) ||
            command.text.contains("delete", ignoreCase = true)
        ) {
            task = task.copy(
                status = TaskStatus.WAITING_FOR_APPROVAL,
                progressPercent = 60,
                summary = "Waiting for explicit approval before a state-changing operation.",
                updatedAtEpochMillis = clock.nowMillis(),
            )
            tasks[taskId] = task
            emit(task.asEvent(correlationId))
            val approval = ApprovalRequest(
                id = idSource.nextId(),
                taskId = taskId,
                title = "Allow simulated state-changing tool?",
                redactedArguments = "delete(path=[redacted workspace path])",
                risk = ApprovalRisk.STATE_CHANGING,
                expiresAtEpochMillis = clock.nowMillis() + 60_000,
                status = ApprovalStatus.PENDING,
                requestedScopes = listOf("tool.filesystem.delete"),
                reason = "The simulated destructive operation requires explicit approval.",
            )
            approvals[approval.id] = approval
            emit(CoreEvent.ApprovalChanged(idSource.nextId(), correlationId, approval))
            return
        }
        stepDelay.await(FakeStep.COMPLETED)

        task = task.copy(
            status = TaskStatus.COMPLETED,
            progressPercent = 100,
            summary = "The simulated agent completed the delegated task deterministically.",
            updatedAtEpochMillis = clock.nowMillis(),
        )
        tasks[taskId] = task
        emit(task.asEvent(correlationId))
        emit(
            CoreEvent.MessageAdded(
                eventId = idSource.nextId(),
                correlationId = correlationId,
                message = ConversationMessage(
                    id = idSource.nextId(),
                    conversationId = activeConversationId,
                    author = MessageAuthor.ASSISTANT,
                    text = "Simulated result: I delegated the request, streamed its progress, and kept the written result in this conversation.",
                    createdAtEpochMillis = clock.nowMillis(),
                    correlationId = correlationId,
                ),
            ),
        )
        voiceSession?.let { session ->
            val speaking = session.copy(
                phase = DialogPhase.SPEAKING,
                ttsPlayback = TtsPlaybackStatus.SPEAKING,
            )
            voiceSession = speaking
            emit(CoreEvent.VoiceChanged(idSource.nextId(), correlationId, speaking))
            emit(
                CoreEvent.TtsSpeak(
                    idSource.nextId(),
                    correlationId,
                    speaking.id,
                    speaking.conversationId,
                    "The simulated agent completed the delegated task.",
                ),
            )
            emit(CoreEvent.TtsChanged(idSource.nextId(), correlationId, TtsPlaybackStatus.SPEAKING))
        }
    }

    private suspend fun startCall(direction: CallDirection, correlationId: String) {
        val activeConversationId = conversationId ?: idSource.nextId().also { conversationId = it }
        val session = VoiceSession(
            id = idSource.nextId(),
            conversationId = activeConversationId,
            direction = direction,
            phase = if (direction == CallDirection.INCOMING) DialogPhase.RINGING else DialogPhase.CONNECTING,
            startedAtEpochMillis = if (direction == CallDirection.INCOMING) null else clock.nowMillis(),
            isMuted = false,
            isPushToTalk = false,
            selectedAudioRoute = AudioRouteKind.EARPIECE,
            ttsPlayback = TtsPlaybackStatus.IDLE,
            isSimulatedMedia = true,
            mediaNotice = "Local simulator: signaling and state only; no microphone audio is transported.",
        )
        voiceSession = session
        val event = if (direction == CallDirection.INCOMING) {
            CoreEvent.IncomingCall(idSource.nextId(), correlationId, session)
        } else {
            CoreEvent.VoiceChanged(idSource.nextId(), correlationId, session)
        }
        emit(event)
        if (direction == CallDirection.OUTGOING) {
            updateVoice(correlationId) { it.copy(phase = DialogPhase.LISTENING) }
            emit(
                CoreEvent.PartialTranscript(
                    idSource.nextId(),
                    correlationId,
                    activeConversationId,
                    "Simulated microphone stream…",
                ),
            )
            emit(
                CoreEvent.FinalTranscript(
                    idSource.nextId(),
                    correlationId,
                    activeConversationId,
                    "This transcript was generated by the local voice simulator.",
                ),
            )
        }
    }

    private suspend fun interruptTts(command: CoreCommand.InterruptTts) {
        updateVoice(command.commandId) {
            it.copy(phase = DialogPhase.INTERRUPTED, ttsPlayback = TtsPlaybackStatus.INTERRUPTED)
        }
        emit(
            CoreEvent.TtsChanged(
                eventId = idSource.nextId(),
                correlationId = command.commandId,
                status = TtsPlaybackStatus.INTERRUPTED,
            ),
        )
        // Deliberately leave every delegated task untouched.
    }

    private suspend fun resolveApproval(command: CoreCommand.ResolveApproval) {
        val existing = approvals[command.approvalId] ?: return
        val updated = existing.copy(
            status = if (command.approved) ApprovalStatus.APPROVED else ApprovalStatus.DENIED,
        )
        approvals[command.approvalId] = updated
        emit(CoreEvent.ApprovalChanged(idSource.nextId(), command.commandId, updated))
        tasks[existing.taskId]?.let { task ->
            val resolvedTask = task.copy(
                status = if (command.approved) TaskStatus.COMPLETED else TaskStatus.CANCELLED,
                progressPercent = if (command.approved) 100 else task.progressPercent,
                summary = if (command.approved) {
                    "The simulated approved operation completed."
                } else {
                    "The operation did not run because approval was denied."
                },
                updatedAtEpochMillis = clock.nowMillis(),
            )
            tasks[task.id] = resolvedTask
            emit(resolvedTask.asEvent(command.commandId))
        }
    }

    private suspend fun cancelTask(command: CoreCommand.CancelTask) {
        val existing = tasks[command.taskId] ?: return
        if (existing.status in TERMINAL_TASK_STATES) return
        val updated = existing.copy(
            status = TaskStatus.CANCELLED,
            summary = "Cancelled by the user.",
            updatedAtEpochMillis = clock.nowMillis(),
        )
        tasks[command.taskId] = updated
        emit(updated.asEvent(command.commandId))
    }

    private suspend fun retryTask(command: CoreCommand.RetryTask) {
        val existing = tasks[command.taskId] ?: return
        val updated = existing.copy(
            status = TaskStatus.QUEUED,
            progressPercent = 0,
            summary = "Queued for an explicit retry.",
            updatedAtEpochMillis = clock.nowMillis(),
        )
        tasks[command.taskId] = updated
        emit(updated.asEvent(command.commandId))
    }

    private suspend fun updateVoice(
        correlationId: String,
        transform: (VoiceSession) -> VoiceSession,
    ) {
        val current = voiceSession ?: return
        val updated = transform(current)
        voiceSession = updated
        emit(CoreEvent.VoiceChanged(idSource.nextId(), correlationId, updated))
    }

    private suspend fun emit(event: CoreEvent) {
        mutableEvents.emit(event)
    }

    private fun TaskRecord.asEvent(correlationId: String) = CoreEvent.TaskChanged(
        eventId = idSource.nextId(),
        correlationId = correlationId,
        task = this,
    )

    companion object {
        const val DEVELOPMENT_PAIRING_CODE = "123456"
        private const val ROTATION_TTL_MILLIS = 5 * 60 * 1_000L

        private val TERMINAL_TASK_STATES = setOf(
            TaskStatus.COMPLETED,
            TaskStatus.PARTIALLY_COMPLETED,
            TaskStatus.FAILED,
            TaskStatus.CANCELLED,
            TaskStatus.TIMED_OUT,
        )
    }
}

private fun commandType(command: CoreCommand): String = when (command) {
    is CoreCommand.SendText -> "conversation.send_text"
    is CoreCommand.SetTranscriptRetention -> "conversation.set_transcript_retention"
    is CoreCommand.StartCall -> "voice.start"
    is CoreCommand.SimulateIncomingCall -> "debug.voice.incoming"
    is CoreCommand.AnswerCall -> "voice.answer"
    is CoreCommand.DeclineCall -> "voice.decline"
    is CoreCommand.EndCall -> "voice.end"
    is CoreCommand.SetMuted -> "voice.set_muted"
    is CoreCommand.SetPushToTalk -> "voice.set_push_to_talk"
    is CoreCommand.SelectAudioRoute -> "voice.select_audio_route"
    is CoreCommand.InterruptTts -> "voice.interrupt_tts"
    is CoreCommand.ResolveApproval -> if (command.approved) "approval.approve" else "approval.deny"
    is CoreCommand.CancelTask -> "task.cancel"
    is CoreCommand.RetryTask -> "task.retry"
}

class PairingException(message: String) : IllegalArgumentException(message)
