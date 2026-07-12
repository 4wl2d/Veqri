package com.veqri.android.ui

import androidx.compose.runtime.Immutable
import com.veqri.android.BuildConfig
import com.veqri.android.data.ApprovalRisk
import com.veqri.android.data.ApprovalStatus
import com.veqri.android.data.AudioRouteKind
import com.veqri.android.data.CallDirection
import com.veqri.android.data.DialogPhase
import com.veqri.android.data.MessageAuthor
import com.veqri.android.data.TaskStatus
import com.veqri.android.data.TtsPlaybackStatus
import kotlinx.collections.immutable.ImmutableList
import kotlinx.collections.immutable.ImmutableSet
import kotlinx.collections.immutable.persistentListOf
import kotlinx.collections.immutable.persistentSetOf

enum class AppDestination(val label: String) {
    CONVERSATION("Conversation"),
    TASKS("Tasks"),
    APPROVALS("Approvals"),
    CALL("Call"),
}

enum class ConnectionTone {
    NEUTRAL,
    GOOD,
    WARNING,
    BAD,
}

@Immutable
data class ConnectionUi(
    val label: String,
    val detail: String,
    val tone: ConnectionTone,
)

@Immutable
data class CredentialRotationUi(
    val label: String = "Ready",
    val detail: String = "The active credential is protected by Android Keystore.",
    val keyVersionLabel: String = "Key version unavailable",
    val buttonLabel: String = "Rotate credential",
    val inProgress: Boolean = false,
    val canRotate: Boolean = true,
)

@Immutable
data class MessageUi(
    val id: String,
    val author: MessageAuthor,
    val authorLabel: String,
    val text: String,
    val timestampLabel: String,
)

@Immutable
data class TaskUi(
    val id: String,
    val title: String,
    val agentLabel: String,
    val status: TaskStatus,
    val statusLabel: String,
    val progressPercent: Int,
    val summary: String,
    val canCancel: Boolean,
    val canRetry: Boolean,
)

@Immutable
data class ApprovalUi(
    val id: String,
    val taskId: String,
    val title: String,
    val redactedArguments: String,
    val risk: ApprovalRisk,
    val riskLabel: String,
    val requestedScopes: ImmutableList<String>,
    val reason: String,
    val expiresLabel: String,
    val status: ApprovalStatus,
)

@Immutable
data class VoiceSessionUi(
    val id: String,
    val direction: CallDirection,
    val phase: DialogPhase,
    val phaseLabel: String,
    val durationLabel: String,
    val isMuted: Boolean,
    val isPushToTalk: Boolean,
    val selectedAudioRoute: AudioRouteKind,
    val availableAudioRoutes: ImmutableSet<AudioRouteKind>,
    val ttsPlayback: TtsPlaybackStatus,
    val isSimulatedMedia: Boolean,
    val mediaNotice: String,
)

@Immutable
data class VeqriUiState(
    val isPaired: Boolean = false,
    val pairingInProgress: Boolean = false,
    val pairingError: String? = null,
    val defaultCoreBaseUrl: String = BuildConfig.DEFAULT_CORE_URL,
    val connection: ConnectionUi = ConnectionUi(
        label = "Offline",
        detail = "Not connected",
        tone = ConnectionTone.NEUTRAL,
    ),
    val destination: AppDestination = AppDestination.CONVERSATION,
    val messages: ImmutableList<MessageUi> = persistentListOf(),
    val tasks: ImmutableList<TaskUi> = persistentListOf(),
    val selectedTask: TaskUi? = null,
    val approvals: ImmutableList<ApprovalUi> = persistentListOf(),
    val partialTranscript: String = "",
    val finalTranscript: String = "",
    val voiceSession: VoiceSessionUi? = null,
    val activeTaskCount: Int = 0,
    val pendingApprovalCount: Int = 0,
    val retainTranscript: Boolean = true,
    val retentionChangeInProgress: Boolean = false,
    val retentionStateAuthoritative: Boolean = true,
    val credentialRotation: CredentialRotationUi = CredentialRotationUi(),
    val globalError: String? = null,
    val isLocalSimulator: Boolean = BuildConfig.USE_FAKE_TRANSPORT,
)

sealed interface VeqriAction {
    data class Pair(val coreBaseUrl: String, val oneTimeCode: String, val deviceName: String) : VeqriAction
    data class Navigate(val destination: AppDestination) : VeqriAction
    data class SendText(val text: String) : VeqriAction
    data object StartCall : VeqriAction
    data object SimulateIncomingCall : VeqriAction
    data class AnswerCall(val sessionId: String) : VeqriAction
    data class DeclineCall(val sessionId: String) : VeqriAction
    data class EndCall(val sessionId: String) : VeqriAction
    data class ToggleMute(val sessionId: String) : VeqriAction
    data class SetPushToTalk(val sessionId: String, val enabled: Boolean) : VeqriAction
    data class SetPushToTalkPressed(val sessionId: String, val pressed: Boolean) : VeqriAction
    data class SelectAudioRoute(val sessionId: String, val route: AudioRouteKind) : VeqriAction
    data class InterruptTts(val sessionId: String) : VeqriAction
    data class SelectTask(val taskId: String?) : VeqriAction
    data class CancelTask(val taskId: String) : VeqriAction
    data class RetryTask(val taskId: String) : VeqriAction
    data class ResolveApproval(val approvalId: String, val approved: Boolean) : VeqriAction
    data class SetTranscriptRetention(val enabled: Boolean) : VeqriAction
    data object RotateCredential : VeqriAction
    data object ForgetLocalDevice : VeqriAction
    data object ClearError : VeqriAction
}
