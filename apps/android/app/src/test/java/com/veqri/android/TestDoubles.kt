package com.veqri.android

import com.veqri.android.call.CallLifecycleController
import com.veqri.android.data.ApprovalRequest
import com.veqri.android.data.AudioRouteKind
import com.veqri.android.data.CacheSnapshot
import com.veqri.android.data.ClientPreferenceStore
import com.veqri.android.data.ClientPreferences
import com.veqri.android.data.ConnectionStatus
import com.veqri.android.data.ConversationCache
import com.veqri.android.data.ConversationMessage
import com.veqri.android.data.CredentialRotationCandidate
import com.veqri.android.data.DeviceCredential
import com.veqri.android.data.DeviceCredentialStore
import com.veqri.android.data.TaskRecord
import com.veqri.android.data.TtsPlaybackStatus
import com.veqri.android.data.VoiceSession
import com.veqri.android.media.AudioRouteController
import com.veqri.android.media.SpeechPlayback
import com.veqri.android.media.SpeechPlaybackState
import com.veqri.android.network.CoreCommand
import com.veqri.android.network.CommandCommitResult
import com.veqri.android.network.CoreEvent
import com.veqri.android.network.CoreTransport
import com.veqri.android.network.CredentialRotationConfirmation
import com.veqri.android.network.CredentialRotationException
import com.veqri.android.network.PairingRequest
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asSharedFlow
import kotlinx.coroutines.flow.asStateFlow

class MemoryCredentialStore : DeviceCredentialStore {
    var value: DeviceCredential? = null
    var rotationCandidate: CredentialRotationCandidate? = null
    val operations = mutableListOf<String>()
    override suspend fun read(): DeviceCredential? = value
    override suspend fun save(credential: DeviceCredential) {
        value = credential
        rotationCandidate = null
        operations += "save-active:${credential.keyVersion}"
    }
    override suspend fun readRotationCandidate(): CredentialRotationCandidate? = rotationCandidate
    override suspend fun saveRotationCandidate(candidate: CredentialRotationCandidate) {
        check(value?.deviceId == candidate.deviceId)
        rotationCandidate = candidate
        operations += "save-candidate:${candidate.keyVersion}"
    }
    override suspend fun promoteRotationCandidate(expectedKeyVersion: Int): DeviceCredential {
        val candidate = checkNotNull(rotationCandidate)
        check(candidate.keyVersion == expectedKeyVersion)
        return candidate.toPromotedCredential().also { promoted ->
            value = promoted
            rotationCandidate = null
            operations += "promote:${promoted.keyVersion}"
        }
    }
    override suspend fun clearRotationCandidate() {
        rotationCandidate = null
        operations += "clear-candidate"
    }
    override suspend fun clear() {
        value = null
        rotationCandidate = null
        operations += "clear-all"
    }
}

class MemoryPreferenceStore : ClientPreferenceStore {
    private val current = MutableStateFlow(
        ClientPreferences("http://10.0.2.2:8080", retainTranscript = true, preferPushToTalk = false),
    )
    override val preferences: Flow<ClientPreferences> = current
	val currentValue: ClientPreferences get() = current.value
	var retainTranscriptWriteFailure: Throwable? = null
    override suspend fun setCoreBaseUrl(value: String) {
        current.value = current.value.copy(coreBaseUrl = value)
    }
	override suspend fun setRetainTranscript(value: Boolean) {
		retainTranscriptWriteFailure?.let { throw it }
		current.value = current.value.copy(retainTranscript = value)
	}
    override suspend fun setPreferPushToTalk(value: Boolean) {
        current.value = current.value.copy(preferPushToTalk = value)
    }
}

class ManualCoreTransport : CoreTransport {
    private val mutableConnection = MutableStateFlow<ConnectionStatus>(ConnectionStatus.Offline)
    private val mutableEvents = MutableSharedFlow<CoreEvent>(extraBufferCapacity = 16)
    val sentCommands = mutableListOf<CoreCommand>()
    val operations = mutableListOf<String>()
    var preparedCandidate: CredentialRotationCandidate? = null
    val confirmationFailures = ArrayDeque<CredentialRotationException>()
    var prepareFailure: CredentialRotationException? = null
    var cancelFailure: CredentialRotationException? = null
	var beforeConfirmResult: suspend (CredentialRotationCandidate) -> Unit = {}
	var commandCommit: suspend (CoreCommand) -> CommandCommitResult = { command ->
		CommandCommitResult(command.commandId, "conversation.set_transcript_retention")
	}
	var connectFailure: Throwable? = null
    var connectedCredential: DeviceCredential? = null
	private var armedCandidate: CredentialRotationCandidate? = null
	@Volatile
	private var awaitingAuthoritativeSnapshot = false

    override val connection: StateFlow<ConnectionStatus> = mutableConnection.asStateFlow()
    override val events: Flow<CoreEvent> = mutableEvents.asSharedFlow()

    override suspend fun pair(request: PairingRequest): Result<DeviceCredential> =
        Result.failure(UnsupportedOperationException("Pairing is not used by this test transport."))

    override suspend fun prepareCredentialRotation(active: DeviceCredential): CredentialRotationCandidate {
        operations += "prepare:${active.keyVersion}"
        prepareFailure?.let { throw it }
        return preparedCandidate ?: CredentialRotationCandidate(
            deviceId = active.deviceId,
            accessToken = "candidate-token",
            coreBaseUrl = active.coreBaseUrl,
            keyVersion = active.keyVersion + 1,
            preparedAtEpochMillis = 1_700_000_000_000,
            expiresAtEpochMillis = 1_700_000_300_000,
            correlationId = "rotation-correlation",
        ).also { preparedCandidate = it }
    }

    override suspend fun armCredentialRotation(candidate: CredentialRotationCandidate?) {
        armedCandidate = candidate
        operations += if (candidate == null) "disarm" else "arm:${candidate.keyVersion}"
    }

    override suspend fun confirmCredentialRotation(
        candidate: CredentialRotationCandidate,
    ): CredentialRotationConfirmation {
        operations += "confirm:${candidate.keyVersion}"
        beforeConfirmResult(candidate)
        if (confirmationFailures.isNotEmpty()) throw confirmationFailures.removeFirst()
        preparedCandidate = null
        return CredentialRotationConfirmation(
            deviceId = candidate.deviceId,
            keyVersion = candidate.keyVersion,
            alreadyConfirmed = false,
            correlationId = candidate.correlationId,
        )
    }

    override suspend fun cancelCredentialRotation(active: DeviceCredential): Boolean {
        operations += "cancel:${active.keyVersion}"
        cancelFailure?.let { throw it }
        val cancelled = preparedCandidate != null
        preparedCandidate = null
        return cancelled
    }

	override suspend fun connect(credential: DeviceCredential) {
		connectFailure?.let { throw it }
		connectedCredential = credential
        operations += "connect:${credential.keyVersion}"
        mutableConnection.value = ConnectionStatus.Connected(CoreTransport.PROTOCOL_VERSION)
    }

    override suspend fun send(command: CoreCommand) {
        sentCommands += command
    }

	override suspend fun sendAwaitingCommit(command: CoreCommand): CommandCommitResult {
		sentCommands += command
		return commandCommit(command)
	}

	override fun isAwaitingAuthoritativeSnapshot(): Boolean = awaitingAuthoritativeSnapshot

    override suspend fun disconnect() {
        operations += "disconnect"
        connectedCredential = null
        mutableConnection.value = ConnectionStatus.Offline
    }

	suspend fun emit(event: CoreEvent) {
		mutableEvents.emit(event)
		if (event is CoreEvent.AuthoritativeSnapshot) awaitingAuthoritativeSnapshot = false
	}

	fun setConnectionForTest(status: ConnectionStatus) {
		if (status is ConnectionStatus.Connecting || status is ConnectionStatus.Reconnecting) {
			awaitingAuthoritativeSnapshot = true
		}
		mutableConnection.value = status
	}

    suspend fun emitCredentialRotationCommitted(keyVersion: Int, correlationId: String = "rotation-correlation") {
        mutableEvents.emit(
            CoreEvent.CredentialRotationCommitted(
                eventId = "rotation-event",
                correlationId = correlationId,
                keyVersion = keyVersion,
            ),
        )
    }
}

class MemoryConversationCache : ConversationCache {
    val messages = linkedMapOf<String, ConversationMessage>()
    val tasks = linkedMapOf<String, TaskRecord>()
    override suspend fun load() = CacheSnapshot(messages.values.toList(), tasks.values.toList())
    override suspend fun replaceAuthoritative(snapshot: CacheSnapshot) {
        messages.clear()
        messages.putAll(snapshot.messages.associateBy(ConversationMessage::id))
        tasks.clear()
        tasks.putAll(snapshot.tasks.associateBy(TaskRecord::id))
    }
    override suspend fun upsert(message: ConversationMessage) {
        messages[message.id] = message
    }
    override suspend fun upsert(task: TaskRecord) {
        tasks[task.id] = task
    }
	override suspend fun deleteTask(taskId: String) {
		tasks.remove(taskId)
	}
	override suspend fun deleteConversation(conversationId: String) {
		messages.entries.removeAll { it.value.conversationId == conversationId }
		tasks.replaceAll { _, task ->
			if (task.conversationId == conversationId) task.copy(goal = "[transcript retention disabled]", summary = "") else task
		}
	}
	override suspend fun clearTranscriptContent() {
		messages.clear()
		tasks.replaceAll { _, task -> task.copy(goal = "[transcript retention disabled]", summary = "") }
	}
    override suspend fun clearAll() {
        messages.clear()
        tasks.clear()
    }
}

class FakeAudioRoutes : AudioRouteController {
    private val routes = MutableStateFlow(setOf(AudioRouteKind.EARPIECE, AudioRouteKind.SPEAKER))
    override val availableRoutes = routes
    override fun start() = Unit
    override fun select(route: AudioRouteKind) = route in routes.value
    override fun stop() = Unit
}

class RecordingSpeechPlayback : SpeechPlayback {
    override val handlesPlayback = true
    private val mutableState = MutableStateFlow(SpeechPlaybackState())
    override val state: StateFlow<SpeechPlaybackState> = mutableState.asStateFlow()
    val spoken = mutableListOf<Pair<String, String>>()
    var stopCount = 0
    var closeCount = 0

    override suspend fun speak(sessionId: String, text: String) {
        spoken += sessionId to text
        mutableState.value = SpeechPlaybackState(TtsPlaybackStatus.SPEAKING, sessionId)
    }

    override suspend fun stop() {
        stopCount++
        mutableState.value = SpeechPlaybackState(TtsPlaybackStatus.INTERRUPTED)
    }

    override fun close() {
        closeCount++
        mutableState.value = SpeechPlaybackState()
    }
}

class RecordingCallController : CallLifecycleController {
    val incoming = mutableListOf<VoiceSession>()
    val active = mutableListOf<VoiceSession>()
    val completed = mutableListOf<TaskRecord>()
    val approvals = mutableListOf<ApprovalRequest>()
    override fun publishIncomingCall(session: VoiceSession) {
        incoming += session
    }
    override fun updateActiveCall(session: VoiceSession) {
        active += session
    }
    override fun endActiveCall() = Unit
    override fun publishTaskCompleted(task: TaskRecord) {
        completed += task
    }
    override fun publishApprovalRequired(approval: ApprovalRequest) {
        approvals += approval
    }
}
