package com.veqri.android.network

import ai.veqri.protocol.v1.DeviceApproval
import ai.veqri.protocol.v1.DeviceApprovalRisk
import ai.veqri.protocol.v1.DeviceApprovalStatus
import ai.veqri.protocol.v1.DeviceAudioRoute
import ai.veqri.protocol.v1.DeviceCallDirection
import ai.veqri.protocol.v1.DeviceCommand
import ai.veqri.protocol.v1.DeviceCommandResult
import ai.veqri.protocol.v1.DeviceCommandResultStatus
import ai.veqri.protocol.v1.DeviceConversationMessage
import ai.veqri.protocol.v1.DeviceDialogPhase
import ai.veqri.protocol.v1.DeviceEvent
import ai.veqri.protocol.v1.DeviceMessageAuthor
import ai.veqri.protocol.v1.DeviceResolveApprovalCommand
import ai.veqri.protocol.v1.DeviceSelectAudioRouteCommand
import ai.veqri.protocol.v1.DeviceSendTextCommand
import ai.veqri.protocol.v1.DeviceSessionCommand
import ai.veqri.protocol.v1.DeviceSetMutedCommand
import ai.veqri.protocol.v1.DeviceSetPushToTalkCommand
import ai.veqri.protocol.v1.DeviceSetTranscriptRetentionCommand
import ai.veqri.protocol.v1.DeviceSnapshot
import ai.veqri.protocol.v1.DeviceStartCallCommand
import ai.veqri.protocol.v1.DeviceTask
import ai.veqri.protocol.v1.DeviceTaskCommand
import ai.veqri.protocol.v1.DeviceTaskStatus
import ai.veqri.protocol.v1.DeviceTranscript
import ai.veqri.protocol.v1.DeviceTtsChanged
import ai.veqri.protocol.v1.DeviceTtsSpeak
import ai.veqri.protocol.v1.DeviceTtsStatus
import ai.veqri.protocol.v1.DeviceVoiceSession
import com.veqri.android.data.ApprovalRequest
import com.veqri.android.data.ApprovalRisk
import com.veqri.android.data.ApprovalStatus
import com.veqri.android.data.AudioRouteKind
import com.veqri.android.data.CallDirection
import com.veqri.android.data.ConversationMessage
import com.veqri.android.data.DialogPhase
import com.veqri.android.data.MessageAuthor
import com.veqri.android.data.TaskRecord
import com.veqri.android.data.TaskStatus
import com.veqri.android.data.TtsPlaybackStatus
import com.veqri.android.data.VoiceSession
import com.veqri.android.protocol.boundTtsTextUtf8
import java.util.Locale
import org.json.JSONObject

/**
 * Typed Android protocol boundary. Protobuf messages are never exposed to the
 * repository, domain state, or Compose; the live wire remains the existing
 * flat snake_case JSON contract.
 */
internal object DeviceJsonCodec {
    fun encode(command: CoreCommand): JSONObject = command.toDeviceProto().toWireJson()

    fun decode(json: JSONObject): CoreEvent {
        val type = json.getString("type")
        val eventId = json.getString("id")
        val correlationId = json.optString("correlation_id", eventId)
        if (type !in knownEventTypes) {
            return CoreEvent.ProtocolError(
                eventId = eventId,
                correlationId = correlationId,
                safeMessage = "Unsupported event type: $type",
            )
        }
        return json.toDeviceEventProto().toDomain()
    }

    private val knownEventTypes = setOf(
        "sync.snapshot",
        "command.result",
        "conversation.message_added",
        "task.changed",
        "approval.changed",
        "transcript.partial",
        "transcript.final",
        "voice.incoming",
        "voice.changed",
        "tts.changed",
        "tts.speak",
    )
}

internal fun parseEvent(json: JSONObject): CoreEvent = DeviceJsonCodec.decode(json)

internal fun CoreCommand.toDeviceProto(): DeviceCommand {
    val envelope = DeviceCommand.newBuilder()
        .setCommandId(commandId)
        .setProtocolVersion(CoreTransport.PROTOCOL_VERSION)
    when (this) {
        is CoreCommand.SendText -> {
            val body = DeviceSendTextCommand.newBuilder()
                .setText(text)
                .setRetainTranscript(retainTranscript)
            conversationId?.let(body::setConversationId)
            envelope.setSendText(body)
        }
        is CoreCommand.SetTranscriptRetention -> {
            val body = DeviceSetTranscriptRetentionCommand.newBuilder().setEnabled(enabled)
            conversationId?.let(body::setConversationId)
            envelope.setSetTranscriptRetention(body)
        }
        is CoreCommand.StartCall -> envelope.setStartCall(
            DeviceStartCallCommand.newBuilder().setRetainTranscript(retainTranscript),
        )
        is CoreCommand.SimulateIncomingCall -> envelope.setSimulateIncomingCall(
            DeviceStartCallCommand.newBuilder().setRetainTranscript(retainTranscript),
        )
        is CoreCommand.AnswerCall -> envelope.setAnswerCall(sessionCommand(sessionId))
        is CoreCommand.DeclineCall -> envelope.setDeclineCall(sessionCommand(sessionId))
        is CoreCommand.EndCall -> envelope.setEndCall(sessionCommand(sessionId))
        is CoreCommand.SetMuted -> envelope.setSetMuted(
            DeviceSetMutedCommand.newBuilder().setSessionId(sessionId).setIsMuted(isMuted),
        )
        is CoreCommand.SetPushToTalk -> envelope.setSetPushToTalk(
            DeviceSetPushToTalkCommand.newBuilder().setSessionId(sessionId).setEnabled(enabled),
        )
        is CoreCommand.SelectAudioRoute -> envelope.setSelectAudioRoute(
            DeviceSelectAudioRouteCommand.newBuilder()
                .setSessionId(sessionId)
                .setRoute(route.toDeviceProto()),
        )
        is CoreCommand.InterruptTts -> envelope.setInterruptTts(sessionCommand(sessionId))
        is CoreCommand.ResolveApproval -> envelope.setResolveApproval(
            DeviceResolveApprovalCommand.newBuilder().setApprovalId(approvalId).setApproved(approved),
        )
        is CoreCommand.CancelTask -> envelope.setCancelTask(taskCommand(taskId))
        is CoreCommand.RetryTask -> envelope.setRetryTask(taskCommand(taskId))
    }
    return envelope.build()
}

private fun sessionCommand(sessionId: String): DeviceSessionCommand.Builder =
    DeviceSessionCommand.newBuilder().setSessionId(sessionId)

private fun taskCommand(taskId: String): DeviceTaskCommand.Builder =
    DeviceTaskCommand.newBuilder().setTaskId(taskId)

internal fun DeviceCommand.toWireJson(): JSONObject {
    val json = JSONObject()
        .put("command_id", commandId)
        .put("protocol_version", protocolVersion)
    when (commandCase) {
        DeviceCommand.CommandCase.SEND_TEXT -> {
            json.put("type", "conversation.send_text")
                .put("text", sendText.text)
                .put("retain_transcript", sendText.retainTranscript)
            if (sendText.hasConversationId()) json.put("conversation_id", sendText.conversationId)
        }
        DeviceCommand.CommandCase.SET_TRANSCRIPT_RETENTION -> {
            json.put("type", "conversation.set_transcript_retention")
                .put("enabled", setTranscriptRetention.enabled)
            if (setTranscriptRetention.hasConversationId()) {
                json.put("conversation_id", setTranscriptRetention.conversationId)
            }
        }
        DeviceCommand.CommandCase.START_CALL -> json
            .put("type", "voice.start")
            .put("retain_transcript", startCall.retainTranscript)
        DeviceCommand.CommandCase.SIMULATE_INCOMING_CALL -> json
            .put("type", "debug.voice.incoming")
            .put("retain_transcript", simulateIncomingCall.retainTranscript)
        DeviceCommand.CommandCase.ANSWER_CALL -> json
            .put("type", "voice.answer")
            .put("session_id", answerCall.sessionId)
        DeviceCommand.CommandCase.DECLINE_CALL -> json
            .put("type", "voice.decline")
            .put("session_id", declineCall.sessionId)
        DeviceCommand.CommandCase.END_CALL -> json
            .put("type", "voice.end")
            .put("session_id", endCall.sessionId)
        DeviceCommand.CommandCase.SET_MUTED -> json
            .put("type", "voice.set_muted")
            .put("session_id", setMuted.sessionId)
            .put("is_muted", setMuted.isMuted)
        DeviceCommand.CommandCase.SET_PUSH_TO_TALK -> json
            .put("type", "voice.set_push_to_talk")
            .put("session_id", setPushToTalk.sessionId)
            .put("enabled", setPushToTalk.enabled)
        DeviceCommand.CommandCase.SELECT_AUDIO_ROUTE -> json
            .put("type", "voice.select_audio_route")
            .put("session_id", selectAudioRoute.sessionId)
            .put("route", selectAudioRoute.route.toWireName())
        DeviceCommand.CommandCase.INTERRUPT_TTS -> json
            .put("type", "voice.interrupt_tts")
            .put("session_id", interruptTts.sessionId)
        DeviceCommand.CommandCase.RESOLVE_APPROVAL -> json
            .put("type", if (resolveApproval.approved) "approval.approve" else "approval.deny")
            .put("approval_id", resolveApproval.approvalId)
        DeviceCommand.CommandCase.CANCEL_TASK -> json
            .put("type", "task.cancel")
            .put("task_id", cancelTask.taskId)
        DeviceCommand.CommandCase.RETRY_TASK -> json
            .put("type", "task.retry")
            .put("task_id", retryTask.taskId)
        DeviceCommand.CommandCase.COMMAND_NOT_SET, null ->
            throw IllegalArgumentException("Device command has no payload.")
    }
    return json
}

internal fun JSONObject.toDeviceEventProto(): DeviceEvent {
    val type = getString("type")
    val eventId = getString("id")
    val correlationId = optString("correlation_id", eventId)
    val payload = optJSONObject("payload") ?: this
    val event = DeviceEvent.newBuilder().setId(eventId).setCorrelationId(correlationId)
    when (type) {
        "sync.snapshot" -> event.setSnapshot(payload.toDeviceSnapshot(correlationId))
        "command.result" -> event.setCommandResult(payload.toDeviceCommandResult())
        "conversation.message_added" -> event.setMessageAdded(
            payload.toDeviceConversationMessage(correlationId),
        )
        "task.changed" -> event.setTaskChanged(payload.toDeviceTask(correlationId))
        "approval.changed" -> event.setApprovalChanged(payload.toDeviceApproval())
        "transcript.partial" -> event.setPartialTranscript(payload.toDeviceTranscript())
        "transcript.final" -> event.setFinalTranscript(payload.toDeviceTranscript())
        "voice.incoming" -> event.setIncomingCall(payload.toDeviceVoiceSession())
        "voice.changed" -> event.setVoiceChanged(payload.toDeviceVoiceSession())
        "tts.changed" -> event.setTtsChanged(
            DeviceTtsChanged.newBuilder().setStatus(deviceTtsStatus(payload.optString("status"))),
        )
        "tts.speak" -> event.setTtsSpeak(
            DeviceTtsSpeak.newBuilder()
                .setSessionId(payload.getString("session_id"))
                .setConversationId(payload.getString("conversation_id"))
                .setStatus(deviceTtsStatus(payload.optString("status")))
                .setText(boundTtsTextUtf8(payload.getString("text"))),
        )
        else -> throw IllegalArgumentException("Unsupported event type: $type")
    }
    return event.build()
}

private fun JSONObject.toDeviceSnapshot(defaultCorrelationId: String): DeviceSnapshot {
    val snapshot = DeviceSnapshot.newBuilder()
        .setSnapshotId(getString("snapshot_id"))
        .setTranscriptRetention(getBoolean("transcript_retention"))
        .addAllMessages(objectList("messages") { item ->
            item.toDeviceConversationMessage(defaultCorrelationId)
        })
        .addAllTasks(objectList("tasks") { item ->
            item.toDeviceTask(defaultCorrelationId)
        })
        .addAllApprovals(objectList("approvals", JSONObject::toDeviceApproval))
    optionalString("conversation_id")?.let(snapshot::setConversationId)
    optJSONObject("voice_session")?.let { snapshot.setVoiceSession(it.toDeviceVoiceSession()) }
    return snapshot.build()
}

private fun JSONObject.toDeviceCommandResult(): DeviceCommandResult {
    val result = DeviceCommandResult.newBuilder()
        .setCommandId(getString("command_id"))
        .setCommandType(getString("command_type"))
        .setStatus(deviceCommandResultStatus(getString("status")))
    optionalString("safe_message")?.let(result::setSafeMessage)
    return result.build()
}

private fun JSONObject.toDeviceConversationMessage(defaultCorrelationId: String): DeviceConversationMessage =
    DeviceConversationMessage.newBuilder()
        .setMessageId(getString("message_id"))
        .setConversationId(getString("conversation_id"))
        .setAuthor(deviceMessageAuthor(optString("author")))
        .setText(getString("text"))
        .setCreatedAtEpochMillis(optLong("created_at_epoch_millis", System.currentTimeMillis()))
        .setCorrelationId(optString("correlation_id", defaultCorrelationId))
        .build()

private fun JSONObject.toDeviceTask(defaultCorrelationId: String): DeviceTask =
    DeviceTask.newBuilder()
        .setTaskId(getString("task_id"))
        .setRootTaskId(optString("root_task_id").ifBlank { getString("task_id") })
        .setConversationId(optString("conversation_id"))
        .setGoal(optString("goal"))
        .setAssignedAgent(optString("assigned_agent", "unassigned"))
        .setStatus(deviceTaskStatus(optString("status")))
        .setProgressPercent(optInt("progress_percent", 0))
        .setSummary(optString("summary"))
        .setCreatedAtEpochMillis(optLong("created_at_epoch_millis"))
        .setUpdatedAtEpochMillis(optLong("updated_at_epoch_millis"))
        .setCorrelationId(optString("correlation_id", defaultCorrelationId))
        .setCanRetry(optBoolean("can_retry", false))
        .setPriority(optInt("priority", 0))
        .setDismissed(optBoolean("dismissed", false))
        .build()

private fun JSONObject.toDeviceApproval(): DeviceApproval =
    DeviceApproval.newBuilder()
        .setApprovalId(getString("approval_id"))
        .setTaskId(getString("task_id"))
        .setTitle(optString("title", "Tool approval required"))
        .setRedactedArguments(optString("redacted_arguments", "Arguments hidden"))
        .setRisk(deviceApprovalRisk(optString("risk")))
        .setExpiresAtEpochMillis(optLong("expires_at_epoch_millis"))
        .setStatus(deviceApprovalStatus(optString("status")))
        .addAllRequestedScopes(stringList("requested_scopes"))
        .setReason(optString("reason"))
        .build()

private fun JSONObject.toDeviceTranscript(): DeviceTranscript =
    DeviceTranscript.newBuilder()
        .setConversationId(getString("conversation_id"))
        .setText(optString("text"))
        .build()

private fun JSONObject.toDeviceVoiceSession(): DeviceVoiceSession {
    val session = DeviceVoiceSession.newBuilder()
        .setSessionId(getString("session_id"))
        .setConversationId(getString("conversation_id"))
        .setDirection(deviceCallDirection(optString("direction")))
        .setPhase(deviceDialogPhase(optString("phase")))
        .setIsMuted(optBoolean("is_muted"))
        .setIsPushToTalk(optBoolean("is_push_to_talk"))
        .setAudioRoute(deviceAudioRoute(optString("audio_route")))
        .setTtsStatus(deviceTtsStatus(optString("tts_status")))
        .setIsSimulatedMedia(optBoolean("is_simulated_media", false))
        .setMediaNotice(optString("media_notice"))
    optLong("started_at_epoch_millis").takeIf { it > 0 }?.let(session::setStartedAtEpochMillis)
    return session.build()
}

internal fun DeviceEvent.toDomain(): CoreEvent = when (eventCase) {
    DeviceEvent.EventCase.SNAPSHOT -> CoreEvent.AuthoritativeSnapshot(
        eventId = id,
        correlationId = correlationId,
        snapshotId = snapshot.snapshotId,
        conversationId = snapshot.conversationId.takeIf { snapshot.hasConversationId() && it.isNotBlank() },
        transcriptRetention = snapshot.transcriptRetention,
        messages = snapshot.messagesList.map(DeviceConversationMessage::toDomain),
        tasks = snapshot.tasksList.map(DeviceTask::toDomain),
        approvals = snapshot.approvalsList.map(DeviceApproval::toDomain),
        voiceSession = snapshot.voiceSession.takeIf { snapshot.hasVoiceSession() }?.toDomain(),
    )
    DeviceEvent.EventCase.COMMAND_RESULT -> {
        require(correlationId == commandResult.commandId) {
            "Command result correlation does not match its command identifier."
        }
        CoreEvent.CommandResult(
            eventId = id,
            correlationId = correlationId,
            commandId = commandResult.commandId,
            commandType = commandResult.commandType,
            committed = commandResult.status.committed(),
            safeMessage = commandResult.safeMessage
                .takeIf { commandResult.hasSafeMessage() && it.isNotBlank() }
                ?.take(240),
        )
    }
    DeviceEvent.EventCase.MESSAGE_ADDED -> CoreEvent.MessageAdded(id, correlationId, messageAdded.toDomain())
    DeviceEvent.EventCase.TASK_CHANGED -> CoreEvent.TaskChanged(id, correlationId, taskChanged.toDomain())
    DeviceEvent.EventCase.APPROVAL_CHANGED -> CoreEvent.ApprovalChanged(
        id,
        correlationId,
        approvalChanged.toDomain(),
    )
    DeviceEvent.EventCase.PARTIAL_TRANSCRIPT -> CoreEvent.PartialTranscript(
        id,
        correlationId,
        partialTranscript.conversationId,
        partialTranscript.text,
    )
    DeviceEvent.EventCase.FINAL_TRANSCRIPT -> CoreEvent.FinalTranscript(
        id,
        correlationId,
        finalTranscript.conversationId,
        finalTranscript.text,
    )
    DeviceEvent.EventCase.INCOMING_CALL -> CoreEvent.IncomingCall(id, correlationId, incomingCall.toDomain())
    DeviceEvent.EventCase.VOICE_CHANGED -> CoreEvent.VoiceChanged(id, correlationId, voiceChanged.toDomain())
    DeviceEvent.EventCase.TTS_CHANGED -> CoreEvent.TtsChanged(
        id,
        correlationId,
        ttsChanged.status.toDomain(),
    )
    DeviceEvent.EventCase.TTS_SPEAK -> CoreEvent.TtsSpeak(
        id,
        correlationId,
        ttsSpeak.sessionId,
        ttsSpeak.conversationId,
        boundTtsTextUtf8(ttsSpeak.text),
    )
    DeviceEvent.EventCase.EVENT_NOT_SET, null ->
        throw IllegalArgumentException("Device event has no payload.")
}

private fun DeviceConversationMessage.toDomain() = ConversationMessage(
    id = messageId,
    conversationId = conversationId,
    author = author.toDomain(),
    text = text,
    createdAtEpochMillis = createdAtEpochMillis,
    correlationId = correlationId,
)

private fun DeviceTask.toDomain(): TaskRecord = TaskRecord(
    id = taskId,
    rootTaskId = rootTaskId.ifBlank { taskId },
    conversationId = conversationId,
    goal = goal,
    assignedAgent = assignedAgent.ifBlank { "unassigned" },
    status = status.toDomain(),
    progressPercent = progressPercent.coerceIn(0, 100),
    summary = summary,
    createdAtEpochMillis = createdAtEpochMillis,
    updatedAtEpochMillis = updatedAtEpochMillis,
    correlationId = correlationId,
    canRetry = canRetry,
    priority = priority.coerceIn(-100, 100),
    dismissed = dismissed,
)

private fun DeviceApproval.toDomain() = ApprovalRequest(
    id = approvalId,
    taskId = taskId,
    title = title,
    redactedArguments = redactedArguments,
    risk = risk.toDomain(),
    expiresAtEpochMillis = expiresAtEpochMillis,
    status = status.toDomain(),
    requestedScopes = requestedScopesList.toList(),
    reason = reason,
)

private fun DeviceVoiceSession.toDomain() = VoiceSession(
    id = sessionId,
    conversationId = conversationId,
    direction = direction.toDomain(),
    phase = phase.toDomain(),
    startedAtEpochMillis = startedAtEpochMillis.takeIf { hasStartedAtEpochMillis() && it > 0 },
    isMuted = isMuted,
    isPushToTalk = isPushToTalk,
    selectedAudioRoute = audioRoute.toDomain(),
    ttsPlayback = ttsStatus.toDomain(),
    isSimulatedMedia = isSimulatedMedia,
    mediaNotice = mediaNotice,
)

private fun AudioRouteKind.toDeviceProto(): DeviceAudioRoute = when (this) {
    AudioRouteKind.EARPIECE -> DeviceAudioRoute.DEVICE_AUDIO_ROUTE_EARPIECE
    AudioRouteKind.SPEAKER -> DeviceAudioRoute.DEVICE_AUDIO_ROUTE_SPEAKER
    AudioRouteKind.WIRED_HEADSET -> DeviceAudioRoute.DEVICE_AUDIO_ROUTE_WIRED_HEADSET
    AudioRouteKind.BLUETOOTH -> DeviceAudioRoute.DEVICE_AUDIO_ROUTE_BLUETOOTH
}

private fun DeviceAudioRoute.toWireName(): String = when (this) {
    DeviceAudioRoute.DEVICE_AUDIO_ROUTE_EARPIECE -> "EARPIECE"
    DeviceAudioRoute.DEVICE_AUDIO_ROUTE_SPEAKER -> "SPEAKER"
    DeviceAudioRoute.DEVICE_AUDIO_ROUTE_WIRED_HEADSET -> "WIRED_HEADSET"
    DeviceAudioRoute.DEVICE_AUDIO_ROUTE_BLUETOOTH -> "BLUETOOTH"
    else -> throw IllegalArgumentException("Device audio route is unspecified.")
}

private fun deviceMessageAuthor(value: String): DeviceMessageAuthor = when (value.wireEnum()) {
    "USER" -> DeviceMessageAuthor.DEVICE_MESSAGE_AUTHOR_USER
    "ASSISTANT" -> DeviceMessageAuthor.DEVICE_MESSAGE_AUTHOR_ASSISTANT
    "SYSTEM" -> DeviceMessageAuthor.DEVICE_MESSAGE_AUTHOR_SYSTEM
    else -> DeviceMessageAuthor.DEVICE_MESSAGE_AUTHOR_UNSPECIFIED
}

private fun DeviceMessageAuthor.toDomain(): MessageAuthor = when (this) {
    DeviceMessageAuthor.DEVICE_MESSAGE_AUTHOR_USER -> MessageAuthor.USER
    DeviceMessageAuthor.DEVICE_MESSAGE_AUTHOR_ASSISTANT -> MessageAuthor.ASSISTANT
    else -> MessageAuthor.SYSTEM
}

private fun deviceTaskStatus(value: String): DeviceTaskStatus =
    DeviceTaskStatus.values().firstOrNull { it.name == "DEVICE_TASK_STATUS_${value.wireEnum()}" }
        ?: DeviceTaskStatus.DEVICE_TASK_STATUS_UNSPECIFIED

private fun DeviceTaskStatus.toDomain(): TaskStatus = when (this) {
    DeviceTaskStatus.DEVICE_TASK_STATUS_QUEUED -> TaskStatus.QUEUED
    DeviceTaskStatus.DEVICE_TASK_STATUS_ASSIGNED -> TaskStatus.ASSIGNED
    DeviceTaskStatus.DEVICE_TASK_STATUS_RUNNING -> TaskStatus.RUNNING
    DeviceTaskStatus.DEVICE_TASK_STATUS_WAITING_FOR_CHILDREN -> TaskStatus.WAITING_FOR_CHILDREN
    DeviceTaskStatus.DEVICE_TASK_STATUS_WAITING_FOR_APPROVAL -> TaskStatus.WAITING_FOR_APPROVAL
    DeviceTaskStatus.DEVICE_TASK_STATUS_BLOCKED -> TaskStatus.BLOCKED
    DeviceTaskStatus.DEVICE_TASK_STATUS_COMPLETED -> TaskStatus.COMPLETED
    DeviceTaskStatus.DEVICE_TASK_STATUS_PARTIALLY_COMPLETED -> TaskStatus.PARTIALLY_COMPLETED
    DeviceTaskStatus.DEVICE_TASK_STATUS_FAILED -> TaskStatus.FAILED
    DeviceTaskStatus.DEVICE_TASK_STATUS_CANCEL_REQUESTED -> TaskStatus.CANCEL_REQUESTED
    DeviceTaskStatus.DEVICE_TASK_STATUS_CANCELLED -> TaskStatus.CANCELLED
    DeviceTaskStatus.DEVICE_TASK_STATUS_TIMED_OUT -> TaskStatus.TIMED_OUT
    else -> TaskStatus.CREATED
}

private fun deviceApprovalRisk(value: String): DeviceApprovalRisk =
    DeviceApprovalRisk.values().firstOrNull { it.name == "DEVICE_APPROVAL_RISK_${value.wireEnum()}" }
        ?: DeviceApprovalRisk.DEVICE_APPROVAL_RISK_UNSPECIFIED

private fun DeviceApprovalRisk.toDomain(): ApprovalRisk = when (this) {
    DeviceApprovalRisk.DEVICE_APPROVAL_RISK_READ_ONLY -> ApprovalRisk.READ_ONLY
    DeviceApprovalRisk.DEVICE_APPROVAL_RISK_LOW -> ApprovalRisk.LOW
    DeviceApprovalRisk.DEVICE_APPROVAL_RISK_DESTRUCTIVE -> ApprovalRisk.DESTRUCTIVE
    DeviceApprovalRisk.DEVICE_APPROVAL_RISK_PRIVILEGED -> ApprovalRisk.PRIVILEGED
    DeviceApprovalRisk.DEVICE_APPROVAL_RISK_EXTERNAL_COMMUNICATION -> ApprovalRisk.EXTERNAL_COMMUNICATION
    DeviceApprovalRisk.DEVICE_APPROVAL_RISK_SECRET_ACCESS -> ApprovalRisk.SECRET_ACCESS
    else -> ApprovalRisk.STATE_CHANGING
}

private fun deviceApprovalStatus(value: String): DeviceApprovalStatus =
    DeviceApprovalStatus.values().firstOrNull { it.name == "DEVICE_APPROVAL_STATUS_${value.wireEnum()}" }
        ?: DeviceApprovalStatus.DEVICE_APPROVAL_STATUS_UNSPECIFIED

private fun DeviceApprovalStatus.toDomain(): ApprovalStatus = when (this) {
    DeviceApprovalStatus.DEVICE_APPROVAL_STATUS_APPROVED -> ApprovalStatus.APPROVED
    DeviceApprovalStatus.DEVICE_APPROVAL_STATUS_DENIED -> ApprovalStatus.DENIED
    DeviceApprovalStatus.DEVICE_APPROVAL_STATUS_EXPIRED -> ApprovalStatus.EXPIRED
    else -> ApprovalStatus.PENDING
}

private fun deviceCallDirection(value: String): DeviceCallDirection =
    DeviceCallDirection.values().firstOrNull { it.name == "DEVICE_CALL_DIRECTION_${value.wireEnum()}" }
        ?: DeviceCallDirection.DEVICE_CALL_DIRECTION_UNSPECIFIED

private fun DeviceCallDirection.toDomain(): CallDirection = when (this) {
    DeviceCallDirection.DEVICE_CALL_DIRECTION_OUTGOING -> CallDirection.OUTGOING
    else -> CallDirection.INCOMING
}

private fun deviceDialogPhase(value: String): DeviceDialogPhase =
    DeviceDialogPhase.values().firstOrNull { it.name == "DEVICE_DIALOG_PHASE_${value.wireEnum()}" }
        ?: DeviceDialogPhase.DEVICE_DIALOG_PHASE_UNSPECIFIED

private fun DeviceDialogPhase.toDomain(): DialogPhase = when (this) {
    DeviceDialogPhase.DEVICE_DIALOG_PHASE_RINGING -> DialogPhase.RINGING
    DeviceDialogPhase.DEVICE_DIALOG_PHASE_CONNECTING -> DialogPhase.CONNECTING
    DeviceDialogPhase.DEVICE_DIALOG_PHASE_LISTENING -> DialogPhase.LISTENING
    DeviceDialogPhase.DEVICE_DIALOG_PHASE_TRANSCRIBING -> DialogPhase.TRANSCRIBING
    DeviceDialogPhase.DEVICE_DIALOG_PHASE_THINKING -> DialogPhase.THINKING
    DeviceDialogPhase.DEVICE_DIALOG_PHASE_DELEGATING -> DialogPhase.DELEGATING
    DeviceDialogPhase.DEVICE_DIALOG_PHASE_WAITING_FOR_RESULT -> DialogPhase.WAITING_FOR_RESULT
    DeviceDialogPhase.DEVICE_DIALOG_PHASE_SPEAKING -> DialogPhase.SPEAKING
    DeviceDialogPhase.DEVICE_DIALOG_PHASE_INTERRUPTED -> DialogPhase.INTERRUPTED
    DeviceDialogPhase.DEVICE_DIALOG_PHASE_WAITING_FOR_APPROVAL -> DialogPhase.WAITING_FOR_APPROVAL
    DeviceDialogPhase.DEVICE_DIALOG_PHASE_RECONNECTING -> DialogPhase.RECONNECTING
    DeviceDialogPhase.DEVICE_DIALOG_PHASE_FAILED -> DialogPhase.FAILED
    DeviceDialogPhase.DEVICE_DIALOG_PHASE_ENDED -> DialogPhase.ENDED
    else -> DialogPhase.IDLE
}

private fun deviceAudioRoute(value: String): DeviceAudioRoute =
    DeviceAudioRoute.values().firstOrNull { it.name == "DEVICE_AUDIO_ROUTE_${value.wireEnum()}" }
        ?: DeviceAudioRoute.DEVICE_AUDIO_ROUTE_UNSPECIFIED

private fun DeviceAudioRoute.toDomain(): AudioRouteKind = when (this) {
    DeviceAudioRoute.DEVICE_AUDIO_ROUTE_SPEAKER -> AudioRouteKind.SPEAKER
    DeviceAudioRoute.DEVICE_AUDIO_ROUTE_WIRED_HEADSET -> AudioRouteKind.WIRED_HEADSET
    DeviceAudioRoute.DEVICE_AUDIO_ROUTE_BLUETOOTH -> AudioRouteKind.BLUETOOTH
    else -> AudioRouteKind.EARPIECE
}

private fun deviceTtsStatus(value: String): DeviceTtsStatus =
    DeviceTtsStatus.values().firstOrNull { it.name == "DEVICE_TTS_STATUS_${value.wireEnum()}" }
        ?: DeviceTtsStatus.DEVICE_TTS_STATUS_UNSPECIFIED

private fun DeviceTtsStatus.toDomain(): TtsPlaybackStatus = when (this) {
    DeviceTtsStatus.DEVICE_TTS_STATUS_BUFFERING -> TtsPlaybackStatus.BUFFERING
    DeviceTtsStatus.DEVICE_TTS_STATUS_SPEAKING -> TtsPlaybackStatus.SPEAKING
    DeviceTtsStatus.DEVICE_TTS_STATUS_INTERRUPTED -> TtsPlaybackStatus.INTERRUPTED
    DeviceTtsStatus.DEVICE_TTS_STATUS_FAILED -> TtsPlaybackStatus.FAILED
    else -> TtsPlaybackStatus.IDLE
}

private fun deviceCommandResultStatus(value: String): DeviceCommandResultStatus = when (value) {
    "COMMITTED" -> DeviceCommandResultStatus.DEVICE_COMMAND_RESULT_STATUS_COMMITTED
    "REJECTED" -> DeviceCommandResultStatus.DEVICE_COMMAND_RESULT_STATUS_REJECTED
    else -> DeviceCommandResultStatus.DEVICE_COMMAND_RESULT_STATUS_UNSPECIFIED
}

private fun DeviceCommandResultStatus.committed(): Boolean = when (this) {
    DeviceCommandResultStatus.DEVICE_COMMAND_RESULT_STATUS_COMMITTED -> true
    DeviceCommandResultStatus.DEVICE_COMMAND_RESULT_STATUS_REJECTED -> false
    else -> throw IllegalArgumentException("Unsupported command result status.")
}

internal fun commandResultCommitted(status: String): Boolean = deviceCommandResultStatus(status).committed()

private inline fun <T> JSONObject.objectList(
    name: String,
    transform: (JSONObject) -> T,
): List<T> {
    val array = optJSONArray(name) ?: return emptyList()
    return buildList(array.length()) {
        for (index in 0 until array.length()) add(transform(array.getJSONObject(index)))
    }
}

private fun JSONObject.stringList(name: String): List<String> {
    val array = optJSONArray(name) ?: return emptyList()
    return buildList(array.length()) {
        for (index in 0 until array.length()) add(array.getString(index))
    }
}

private fun JSONObject.optionalString(name: String): String? =
    if (!has(name) || isNull(name)) null else getString(name).takeIf(String::isNotBlank)

private fun String.wireEnum(): String = uppercase(Locale.US)
