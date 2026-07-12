package com.veqri.android.network

import com.veqri.android.data.ApprovalRequest
import com.veqri.android.data.AudioRouteKind
import com.veqri.android.data.ConnectionStatus
import com.veqri.android.data.ConversationMessage
import com.veqri.android.data.CredentialRotationCandidate
import com.veqri.android.data.DeviceCredential
import com.veqri.android.data.TaskRecord
import com.veqri.android.data.TtsPlaybackStatus
import com.veqri.android.data.VoiceSession
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.StateFlow

data class PairingRequest(
    val coreBaseUrl: String,
    val oneTimeCode: String,
    val deviceName: String,
    val retainTranscript: Boolean = true,
    val clientProtocolVersion: Int = CoreTransport.PROTOCOL_VERSION,
)

data class CredentialRotationConfirmation(
    val deviceId: String,
    val keyVersion: Int,
    val alreadyConfirmed: Boolean,
    val correlationId: String,
)

enum class CredentialRotationFailureKind {
    EXPIRED,
    PENDING,
    UNAUTHORIZED,
    TRANSIENT,
    INVALID_RESPONSE,
}

class CredentialRotationException(
    val kind: CredentialRotationFailureKind,
    message: String,
    cause: Throwable? = null,
) : IllegalStateException(message, cause)

data class CommandCommitResult(
    val commandId: String,
    val commandType: String,
)

enum class CommandCommitFailureKind {
    REJECTED,
    OUTCOME_UNKNOWN,
}

class CommandCommitException(
    val kind: CommandCommitFailureKind,
    message: String,
    cause: Throwable? = null,
) : IllegalStateException(message, cause)

sealed interface CoreEvent {
    val eventId: String
    val correlationId: String

    data class MessageAdded(
        override val eventId: String,
        override val correlationId: String,
        val message: ConversationMessage,
    ) : CoreEvent

    data class TaskChanged(
        override val eventId: String,
        override val correlationId: String,
        val task: TaskRecord,
    ) : CoreEvent

    data class ApprovalChanged(
        override val eventId: String,
        override val correlationId: String,
        val approval: ApprovalRequest,
    ) : CoreEvent

    /**
     * A complete, bounded replacement for Android-visible durable state.
     * Normal events received after this snapshot are ordered live deltas.
     */
    data class AuthoritativeSnapshot(
        override val eventId: String,
        override val correlationId: String,
        val snapshotId: String,
        val conversationId: String?,
        val transcriptRetention: Boolean,
        val messages: List<ConversationMessage>,
        val tasks: List<TaskRecord>,
        val approvals: List<ApprovalRequest>,
        val voiceSession: VoiceSession?,
    ) : CoreEvent

    /** Internal transport proof that Core committed the armed replacement. */
    data class CredentialRotationCommitted(
        override val eventId: String,
        override val correlationId: String,
        val keyVersion: Int,
    ) : CoreEvent

    /** Correlated proof that Core either committed or rejected one command. */
    data class CommandResult(
        override val eventId: String,
        override val correlationId: String,
        val commandId: String,
        val commandType: String,
        val committed: Boolean,
        val safeMessage: String?,
    ) : CoreEvent

    data class PartialTranscript(
        override val eventId: String,
        override val correlationId: String,
        val conversationId: String,
        val text: String,
    ) : CoreEvent

    data class FinalTranscript(
        override val eventId: String,
        override val correlationId: String,
        val conversationId: String,
        val text: String,
    ) : CoreEvent

    data class VoiceChanged(
        override val eventId: String,
        override val correlationId: String,
        val session: VoiceSession,
    ) : CoreEvent

    data class TtsChanged(
        override val eventId: String,
        override val correlationId: String,
        val status: TtsPlaybackStatus,
    ) : CoreEvent

    /** One bounded logical response for on-device playback; streaming chunks are status-only. */
    data class TtsSpeak(
        override val eventId: String,
        override val correlationId: String,
        val sessionId: String,
        val conversationId: String,
        val text: String,
    ) : CoreEvent

    data class IncomingCall(
        override val eventId: String,
        override val correlationId: String,
        val session: VoiceSession,
    ) : CoreEvent

    data class ProtocolError(
        override val eventId: String,
        override val correlationId: String,
        val safeMessage: String,
    ) : CoreEvent
}

sealed interface CoreCommand {
    val commandId: String

    data class SendText(
        override val commandId: String,
        val conversationId: String?,
        val text: String,
        val retainTranscript: Boolean = true,
    ) : CoreCommand

	data class SetTranscriptRetention(
		override val commandId: String,
		val conversationId: String?,
		val enabled: Boolean,
	) : CoreCommand

	data class StartCall(override val commandId: String, val retainTranscript: Boolean = true) : CoreCommand
	data class SimulateIncomingCall(override val commandId: String, val retainTranscript: Boolean = true) : CoreCommand
    data class AnswerCall(override val commandId: String, val sessionId: String) : CoreCommand
    data class DeclineCall(override val commandId: String, val sessionId: String) : CoreCommand
    data class EndCall(override val commandId: String, val sessionId: String) : CoreCommand
    data class SetMuted(
        override val commandId: String,
        val sessionId: String,
        val isMuted: Boolean,
    ) : CoreCommand

    data class SetPushToTalk(
        override val commandId: String,
        val sessionId: String,
        val enabled: Boolean,
    ) : CoreCommand

    data class SelectAudioRoute(
        override val commandId: String,
        val sessionId: String,
        val route: AudioRouteKind,
    ) : CoreCommand

    data class InterruptTts(
        override val commandId: String,
        val sessionId: String,
    ) : CoreCommand

    data class ResolveApproval(
        override val commandId: String,
        val approvalId: String,
        val approved: Boolean,
    ) : CoreCommand

    data class CancelTask(override val commandId: String, val taskId: String) : CoreCommand
    data class RetryTask(override val commandId: String, val taskId: String) : CoreCommand
}

interface CoreTransport {
    val connection: StateFlow<ConnectionStatus>
    val events: Flow<CoreEvent>

    suspend fun pair(request: PairingRequest): Result<DeviceCredential>
    suspend fun prepareCredentialRotation(active: DeviceCredential): CredentialRotationCandidate
    suspend fun armCredentialRotation(candidate: CredentialRotationCandidate?)
    suspend fun confirmCredentialRotation(candidate: CredentialRotationCandidate): CredentialRotationConfirmation
    suspend fun cancelCredentialRotation(active: DeviceCredential): Boolean
    suspend fun connect(credential: DeviceCredential)
    suspend fun send(command: CoreCommand)
    /** Sends exactly once and waits for Core's correlated commit result. */
    suspend fun sendAwaitingCommit(command: CoreCommand): CommandCommitResult
    /** True from socket open until its first authoritative snapshot is applied. */
    fun isAwaitingAuthoritativeSnapshot(): Boolean = false
    suspend fun disconnect()

    companion object {
        const val PROTOCOL_VERSION = 1
        const val MAX_TTS_TEXT_CHARS = 12 * 1024
    }
}

fun interface IdSource {
    fun nextId(): String
}

fun interface EpochClock {
    fun nowMillis(): Long
}

fun interface FakeStepDelay {
    suspend fun await(step: FakeStep)
}

enum class FakeStep {
    ACCEPTED,
    DELEGATED,
    COMPLETED,
    RECONNECTED,
}
