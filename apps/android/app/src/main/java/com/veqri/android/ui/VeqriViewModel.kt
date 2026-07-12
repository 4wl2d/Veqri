package com.veqri.android.ui

import androidx.lifecycle.ViewModel
import androidx.lifecycle.ViewModelProvider
import androidx.lifecycle.viewModelScope
import com.veqri.android.data.ApprovalStatus
import com.veqri.android.data.ClientSnapshot
import com.veqri.android.data.ConnectionStatus
import com.veqri.android.data.ConversationMessage
import com.veqri.android.data.CredentialRotationPhase
import com.veqri.android.data.DialogPhase
import com.veqri.android.data.MessageAuthor
import com.veqri.android.data.TaskRecord
import com.veqri.android.data.TaskStatus
import com.veqri.android.data.VeqriRepository
import java.text.DateFormat
import java.util.Date
import kotlinx.collections.immutable.toImmutableList
import kotlinx.collections.immutable.toImmutableSet
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharingStarted
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.flow.collectLatest
import kotlinx.coroutines.flow.distinctUntilChanged
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.flow.stateIn
import kotlinx.coroutines.currentCoroutineContext
import kotlinx.coroutines.channels.Channel
import kotlinx.coroutines.delay
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch

class VeqriViewModel(
    private val repository: VeqriRepository,
) : ViewModel() {
    private val destination = MutableStateFlow(AppDestination.CONVERSATION)
    private val selectedTaskId = MutableStateFlow<String?>(null)
    private val nowEpochMillis = MutableStateFlow(System.currentTimeMillis())
	private val actionQueue = Channel<VeqriAction>(capacity = Channel.UNLIMITED)

    val uiState = combine(
        repository.snapshot,
        destination,
        selectedTaskId,
        nowEpochMillis,
    ) { snapshot, target, taskId, now ->
        snapshot.toUiState(target, taskId, now)
    }.stateIn(
        scope = viewModelScope,
        started = SharingStarted.WhileSubscribed(stopTimeoutMillis = 5_000),
        initialValue = VeqriUiState(),
    )

    init {
		viewModelScope.launch {
			for (action in actionQueue) execute(action)
		}
        viewModelScope.launch {
            repository.snapshot
                .map { snapshot ->
                    snapshot.voiceSession?.startedAtEpochMillis != null &&
                        snapshot.voiceSession.phase != DialogPhase.ENDED
                }
                .distinctUntilChanged()
                .collectLatest { hasActiveTimer ->
                    if (!hasActiveTimer) return@collectLatest
                    while (currentCoroutineContext().isActive) {
                        nowEpochMillis.value = System.currentTimeMillis()
                        delay(1_000)
                    }
                }
        }
    }

    fun dispatch(action: VeqriAction) {
        when (action) {
            is VeqriAction.Navigate -> destination.value = action.destination
            is VeqriAction.SelectTask -> {
                selectedTaskId.value = action.taskId
                destination.value = AppDestination.TASKS
            }
			else -> actionQueue.trySend(action)
        }
    }

    private suspend fun execute(action: VeqriAction) {
        when (action) {
            is VeqriAction.Pair -> repository.pair(
                coreBaseUrl = action.coreBaseUrl,
                oneTimeCode = action.oneTimeCode,
                deviceName = action.deviceName,
            )
            is VeqriAction.SendText -> repository.sendText(action.text)
            VeqriAction.StartCall -> {
                destination.value = AppDestination.CALL
                repository.startCall()
            }
            VeqriAction.SimulateIncomingCall -> {
                destination.value = AppDestination.CALL
                repository.simulateIncomingCall()
            }
            is VeqriAction.AnswerCall -> {
                destination.value = AppDestination.CALL
                repository.answerCall(action.sessionId)
            }
            is VeqriAction.DeclineCall -> repository.declineCall(action.sessionId)
            is VeqriAction.EndCall -> repository.endCall(action.sessionId)
            is VeqriAction.ToggleMute -> repository.toggleMute(action.sessionId)
            is VeqriAction.SetPushToTalk -> repository.setPushToTalk(action.sessionId, action.enabled)
            is VeqriAction.SetPushToTalkPressed -> repository.setPushToTalkPressed(
                action.sessionId,
                action.pressed,
            )
            is VeqriAction.SelectAudioRoute -> repository.selectAudioRoute(action.sessionId, action.route)
            is VeqriAction.InterruptTts -> repository.interruptTts(action.sessionId)
            is VeqriAction.CancelTask -> repository.cancelTask(action.taskId)
            is VeqriAction.RetryTask -> repository.retryTask(action.taskId)
            is VeqriAction.ResolveApproval -> repository.resolveApproval(action.approvalId, action.approved)
            is VeqriAction.SetTranscriptRetention -> repository.setRetainTranscript(action.enabled)
            VeqriAction.RotateCredential -> repository.rotateCredential()
            VeqriAction.ForgetLocalDevice -> repository.forgetLocalDevice()
            VeqriAction.ClearError -> repository.clearError()
            is VeqriAction.Navigate, is VeqriAction.SelectTask -> Unit
        }
    }

    class Factory(private val repository: VeqriRepository) : ViewModelProvider.Factory {
        @Suppress("UNCHECKED_CAST")
        override fun <T : ViewModel> create(modelClass: Class<T>): T {
            require(modelClass.isAssignableFrom(VeqriViewModel::class.java))
            return VeqriViewModel(repository) as T
        }
    }
}

private fun ClientSnapshot.toUiState(
    destination: AppDestination,
    selectedTaskId: String?,
    nowEpochMillis: Long,
): VeqriUiState {
    val taskModels = tasks.values
        .sortedWith(
            compareByDescending<TaskRecord> { it.priority }
                .thenByDescending { it.updatedAtEpochMillis },
        )
        .map(TaskRecord::toUi)
        .toImmutableList()
    val voice = voiceSession
    return VeqriUiState(
        isPaired = isPaired,
        pairingInProgress = pairingInProgress,
        pairingError = pairingError,
        connection = connection.toUi(),
        destination = destination,
        messages = messages.sortedBy(ConversationMessage::createdAtEpochMillis)
            .map(ConversationMessage::toUi)
            .toImmutableList(),
        tasks = taskModels,
        selectedTask = taskModels.firstOrNull { it.id == selectedTaskId },
        approvals = approvals.values.sortedBy { it.expiresAtEpochMillis }.map { approval ->
            ApprovalUi(
                id = approval.id,
                taskId = approval.taskId,
                title = approval.title,
                redactedArguments = approval.redactedArguments,
                risk = approval.risk,
                riskLabel = approval.risk.name.lowercase().replace('_', ' '),
                requestedScopes = approval.requestedScopes.toImmutableList(),
                reason = approval.reason,
                expiresLabel = if (approval.expiresAtEpochMillis > 0) {
                    DateFormat.getTimeInstance(DateFormat.SHORT).format(Date(approval.expiresAtEpochMillis))
                } else {
                    "Core-controlled expiry"
                },
                status = approval.status,
            )
        }.toImmutableList(),
        partialTranscript = partialTranscript,
        finalTranscript = finalTranscript,
        voiceSession = voice?.let { session ->
            VoiceSessionUi(
                id = session.id,
                direction = session.direction,
                phase = session.phase,
                phaseLabel = session.phase.name.lowercase().replace('_', ' '),
                durationLabel = session.startedAtEpochMillis?.let { started ->
                    formatDuration((nowEpochMillis - started).coerceAtLeast(0))
                } ?: "00:00",
                isMuted = session.isMuted,
                isPushToTalk = session.isPushToTalk,
                selectedAudioRoute = session.selectedAudioRoute,
                availableAudioRoutes = availableAudioRoutes.toImmutableSet(),
                ttsPlayback = session.ttsPlayback,
                isSimulatedMedia = session.isSimulatedMedia,
                mediaNotice = session.mediaNotice,
            )
        },
        activeTaskCount = tasks.values.count { it.status in ACTIVE_TASK_STATES },
        pendingApprovalCount = approvals.values.count { it.status == ApprovalStatus.PENDING },
        retainTranscript = retainTranscript,
        retentionChangeInProgress = retentionChangeInProgress,
        retentionStateAuthoritative = retentionStateAuthoritative,
        credentialRotation = CredentialRotationUi(
            label = when (credentialRotation.phase) {
                CredentialRotationPhase.IDLE -> "Ready"
                CredentialRotationPhase.PREPARING -> "Preparing replacement"
                CredentialRotationPhase.CONFIRMING -> "Confirming replacement"
                CredentialRotationPhase.RECOVERING -> "Recovering rotation"
                CredentialRotationPhase.SUCCEEDED -> "Rotation complete"
                CredentialRotationPhase.FAILED -> "Rotation needs attention"
            },
            detail = credentialRotation.message,
            keyVersionLabel = if (credentialKeyVersion > 0) {
                "Active key version $credentialKeyVersion"
            } else {
                "Key version unavailable"
            },
            buttonLabel = if (credentialRotation.phase == CredentialRotationPhase.FAILED) {
                "Resume rotation"
            } else {
                "Rotate credential"
            },
            inProgress = credentialRotation.phase in setOf(
                CredentialRotationPhase.PREPARING,
                CredentialRotationPhase.CONFIRMING,
                CredentialRotationPhase.RECOVERING,
            ),
            canRotate = isPaired && credentialRotation.phase !in setOf(
                CredentialRotationPhase.PREPARING,
                CredentialRotationPhase.CONFIRMING,
                CredentialRotationPhase.RECOVERING,
            ),
        ),
        globalError = errorMessage,
    )
}

private fun ConnectionStatus.toUi(): ConnectionUi = when (this) {
    ConnectionStatus.Offline -> ConnectionUi("Offline", "No Core connection", ConnectionTone.NEUTRAL)
    ConnectionStatus.Connecting -> ConnectionUi("Connecting", "Opening authenticated stream", ConnectionTone.WARNING)
    is ConnectionStatus.Connected -> ConnectionUi("Connected", "Protocol v$protocolVersion", ConnectionTone.GOOD)
    is ConnectionStatus.Reconnecting -> ConnectionUi(
        "Reconnecting",
        "Attempt $attempt in ${retryInMillis} ms",
        ConnectionTone.WARNING,
    )
    is ConnectionStatus.Failed -> ConnectionUi("Connection failed", userMessage, ConnectionTone.BAD)
}

private fun ConversationMessage.toUi() = MessageUi(
    id = id,
    author = author,
    authorLabel = when (author) {
        MessageAuthor.USER -> "You"
        MessageAuthor.ASSISTANT -> "Veqri"
        MessageAuthor.SYSTEM -> "System"
    },
    text = text,
    timestampLabel = DateFormat.getTimeInstance(DateFormat.SHORT).format(Date(createdAtEpochMillis)),
)

private fun TaskRecord.toUi() = TaskUi(
    id = id,
    title = goal,
    agentLabel = assignedAgent,
    status = status,
    statusLabel = status.name.lowercase().replace('_', ' '),
    progressPercent = progressPercent,
    summary = summary,
    canCancel = status in ACTIVE_TASK_STATES,
    canRetry = canRetry,
)

private fun formatDuration(milliseconds: Long): String {
    val totalSeconds = milliseconds / 1_000
    return "%02d:%02d".format(totalSeconds / 60, totalSeconds % 60)
}

private val ACTIVE_TASK_STATES = setOf(
    TaskStatus.CREATED,
    TaskStatus.QUEUED,
    TaskStatus.ASSIGNED,
    TaskStatus.RUNNING,
    TaskStatus.WAITING_FOR_CHILDREN,
    TaskStatus.WAITING_FOR_APPROVAL,
    TaskStatus.BLOCKED,
    TaskStatus.CANCEL_REQUESTED,
)
