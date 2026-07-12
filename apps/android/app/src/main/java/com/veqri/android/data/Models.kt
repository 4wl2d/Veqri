package com.veqri.android.data

enum class MessageAuthor {
    USER,
    ASSISTANT,
    SYSTEM,
}

data class ConversationMessage(
    val id: String,
    val conversationId: String,
    val author: MessageAuthor,
    val text: String,
    val createdAtEpochMillis: Long,
    val correlationId: String,
)

enum class TaskStatus {
    CREATED,
    QUEUED,
    ASSIGNED,
    RUNNING,
    WAITING_FOR_CHILDREN,
    WAITING_FOR_APPROVAL,
    BLOCKED,
    COMPLETED,
    PARTIALLY_COMPLETED,
    FAILED,
    CANCEL_REQUESTED,
    CANCELLED,
    TIMED_OUT,
}

data class TaskRecord(
    val id: String,
    val rootTaskId: String,
    val conversationId: String,
    val goal: String,
    val assignedAgent: String,
    val status: TaskStatus,
    val progressPercent: Int,
    val summary: String,
    val createdAtEpochMillis: Long,
    val updatedAtEpochMillis: Long,
    val correlationId: String,
    /** Core-computed eligibility; false is the safe offline/default value. */
    val canRetry: Boolean = false,
    val priority: Int = 0,
    val dismissed: Boolean = false,
)

enum class ApprovalRisk {
    READ_ONLY,
    LOW,
    STATE_CHANGING,
    DESTRUCTIVE,
    PRIVILEGED,
    EXTERNAL_COMMUNICATION,
    SECRET_ACCESS,
}

enum class ApprovalStatus {
    PENDING,
    APPROVED,
    DENIED,
    EXPIRED,
}

data class ApprovalRequest(
    val id: String,
    val taskId: String,
    val title: String,
    val redactedArguments: String,
    val risk: ApprovalRisk,
    val expiresAtEpochMillis: Long,
    val status: ApprovalStatus,
    val requestedScopes: List<String> = emptyList(),
    val reason: String = "",
)

enum class DialogPhase {
    IDLE,
    RINGING,
    CONNECTING,
    LISTENING,
    TRANSCRIBING,
    THINKING,
    DELEGATING,
    WAITING_FOR_RESULT,
    SPEAKING,
    INTERRUPTED,
    WAITING_FOR_APPROVAL,
    RECONNECTING,
    FAILED,
    ENDED,
}

enum class CallDirection {
    INCOMING,
    OUTGOING,
}

enum class AudioRouteKind {
    EARPIECE,
    SPEAKER,
    WIRED_HEADSET,
    BLUETOOTH,
}

enum class TtsPlaybackStatus {
    IDLE,
    BUFFERING,
    SPEAKING,
    INTERRUPTED,
    FAILED,
}

data class VoiceSession(
    val id: String,
    val conversationId: String,
    val direction: CallDirection,
    val phase: DialogPhase,
    val startedAtEpochMillis: Long?,
    val isMuted: Boolean,
    val isPushToTalk: Boolean,
    val selectedAudioRoute: AudioRouteKind,
    val ttsPlayback: TtsPlaybackStatus,
    val isSimulatedMedia: Boolean,
    val mediaNotice: String,
)

sealed interface ConnectionStatus {
    data object Offline : ConnectionStatus
    data object Connecting : ConnectionStatus
    data class Connected(val protocolVersion: Int) : ConnectionStatus
    data class Reconnecting(val attempt: Int, val retryInMillis: Long) : ConnectionStatus
    data class Failed(val userMessage: String) : ConnectionStatus
}

data class DeviceCredential(
    val deviceId: String,
    val accessToken: String,
    val coreBaseUrl: String,
    val issuedAtEpochMillis: Long,
    val keyVersion: Int = 1,
) {
    override fun toString(): String =
        "DeviceCredential(deviceId=$deviceId, accessToken=[redacted], coreBaseUrl=$coreBaseUrl, " +
            "issuedAtEpochMillis=$issuedAtEpochMillis, keyVersion=$keyVersion)"
}

data class CredentialRotationCandidate(
    val deviceId: String,
    val accessToken: String,
    val coreBaseUrl: String,
    val keyVersion: Int,
    val preparedAtEpochMillis: Long,
    val expiresAtEpochMillis: Long,
    val correlationId: String,
) {
    fun toPromotedCredential() = DeviceCredential(
        deviceId = deviceId,
        accessToken = accessToken,
        coreBaseUrl = coreBaseUrl,
        issuedAtEpochMillis = preparedAtEpochMillis,
        keyVersion = keyVersion,
    )

    override fun toString(): String =
        "CredentialRotationCandidate(deviceId=$deviceId, accessToken=[redacted], " +
            "coreBaseUrl=$coreBaseUrl, keyVersion=$keyVersion, " +
            "preparedAtEpochMillis=$preparedAtEpochMillis, expiresAtEpochMillis=$expiresAtEpochMillis, " +
            "correlationId=$correlationId)"
}

enum class CredentialRotationPhase {
    IDLE,
    PREPARING,
    CONFIRMING,
    RECOVERING,
    SUCCEEDED,
    FAILED,
}

data class CredentialRotationState(
    val phase: CredentialRotationPhase = CredentialRotationPhase.IDLE,
    val targetKeyVersion: Int? = null,
    val message: String = "The active credential is protected by Android Keystore.",
)

data class ClientPreferences(
    val coreBaseUrl: String,
    val retainTranscript: Boolean,
    val preferPushToTalk: Boolean,
)

data class ClientSnapshot(
    val isPaired: Boolean = false,
    val pairingInProgress: Boolean = false,
    val pairingError: String? = null,
    val connection: ConnectionStatus = ConnectionStatus.Offline,
    val conversationId: String? = null,
    val messages: List<ConversationMessage> = emptyList(),
    val tasks: Map<String, TaskRecord> = emptyMap(),
    val approvals: Map<String, ApprovalRequest> = emptyMap(),
    val partialTranscript: String = "",
    val finalTranscript: String = "",
    val voiceSession: VoiceSession? = null,
    val availableAudioRoutes: Set<AudioRouteKind> = setOf(
        AudioRouteKind.EARPIECE,
        AudioRouteKind.SPEAKER,
    ),
    val retainTranscript: Boolean = true,
    val retentionChangeInProgress: Boolean = false,
    val retentionStateAuthoritative: Boolean = true,
    val credentialKeyVersion: Int = 0,
    val credentialRotation: CredentialRotationState = CredentialRotationState(),
    val errorMessage: String? = null,
)
