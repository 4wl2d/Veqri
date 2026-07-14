package com.veqri.android.network

import ai.veqri.protocol.v1.DeviceCommand
import ai.veqri.protocol.v1.DeviceEvent
import ai.veqri.protocol.v1.DeviceTtsStatus
import com.veqri.android.data.ApprovalRisk
import com.veqri.android.data.ApprovalStatus
import com.veqri.android.data.AudioRouteKind
import com.veqri.android.data.CallDirection
import com.veqri.android.data.DialogPhase
import com.veqri.android.data.MessageAuthor
import com.veqri.android.data.TaskStatus
import com.veqri.android.data.TtsPlaybackStatus
import com.veqri.android.protocol.MAX_TTS_TEXT_UTF8_BYTES
import com.veqri.android.protocol.utf8Size
import org.json.JSONArray
import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertThrows
import org.junit.Assert.assertTrue
import org.junit.Test

class DeviceJsonCodecTest {
    @Test
    fun `all commands pass through generated oneofs and preserve flat json wire`() {
        val cases = listOf(
            CommandCase(
                CoreCommand.SendText("send", null, "Привет 😀", retainTranscript = false),
                "conversation.send_text",
                mapOf("text" to "Привет 😀", "retain_transcript" to false),
                absent = setOf("conversation_id"),
            ),
            CommandCase(
                CoreCommand.SetTranscriptRetention("retention", "conversation", enabled = true),
                "conversation.set_transcript_retention",
                mapOf("conversation_id" to "conversation", "enabled" to true),
            ),
            CommandCase(CoreCommand.StartCall("start", retainTranscript = false), "voice.start", mapOf("retain_transcript" to false)),
            CommandCase(
                CoreCommand.SimulateIncomingCall("incoming", retainTranscript = true),
                "debug.voice.incoming",
                mapOf("retain_transcript" to true),
            ),
            CommandCase(CoreCommand.AnswerCall("answer", "session"), "voice.answer", mapOf("session_id" to "session")),
            CommandCase(CoreCommand.DeclineCall("decline", "session"), "voice.decline", mapOf("session_id" to "session")),
            CommandCase(CoreCommand.EndCall("end", "session"), "voice.end", mapOf("session_id" to "session")),
            CommandCase(
                CoreCommand.SetMuted("mute", "session", isMuted = false),
                "voice.set_muted",
                mapOf("session_id" to "session", "is_muted" to false),
            ),
            CommandCase(
                CoreCommand.SetPushToTalk("ptt", "session", enabled = true),
                "voice.set_push_to_talk",
                mapOf("session_id" to "session", "enabled" to true),
            ),
            CommandCase(
                CoreCommand.SelectAudioRoute("route", "session", AudioRouteKind.SPEAKER),
                "voice.select_audio_route",
                mapOf("session_id" to "session", "route" to "SPEAKER"),
            ),
            CommandCase(
                CoreCommand.InterruptTts("interrupt", "session"),
                "voice.interrupt_tts",
                mapOf("session_id" to "session"),
            ),
            CommandCase(
                CoreCommand.ResolveApproval("approve", "approval", approved = true),
                "approval.approve",
                mapOf("approval_id" to "approval"),
            ),
            CommandCase(
                CoreCommand.ResolveApproval("deny", "approval", approved = false),
                "approval.deny",
                mapOf("approval_id" to "approval"),
            ),
            CommandCase(CoreCommand.CancelTask("cancel", "task"), "task.cancel", mapOf("task_id" to "task")),
            CommandCase(CoreCommand.RetryTask("retry", "task"), "task.retry", mapOf("task_id" to "task")),
        )

        assertEquals(15, cases.map(CommandCase::type).distinct().size)
        cases.forEach { case ->
            val proto = case.command.toDeviceProto()
            val reparsed = DeviceCommand.parseFrom(proto.toByteArray())
            assertEquals(proto.commandCase, reparsed.commandCase)

            val json = DeviceJsonCodec.encode(case.command)
            assertEquals(case.command.commandId, json.getString("command_id"))
            assertEquals(CoreTransport.PROTOCOL_VERSION, json.getInt("protocol_version"))
            assertEquals(case.type, json.getString("type"))
            case.fields.forEach { (name, expected) -> assertEquals(expected, json.get(name)) }
            assertEquals(
                setOf("command_id", "protocol_version", "type") + case.fields.keys,
                json.keys().asSequence().toSet(),
            )
            case.absent.forEach { name -> assertFalse(json.has(name)) }
            assertFalse(json.has("sendText"))
            assertFalse(json.has("command"))
        }
    }

    @Test
    fun `all audio routes retain their json spelling`() {
        AudioRouteKind.entries.forEach { route ->
            val json = DeviceJsonCodec.encode(CoreCommand.SelectAudioRoute("id", "session", route))
            assertEquals(route.name, json.getString("route"))
        }
    }

    @Test
    fun `authoritative snapshot passes through generated event and maps to immutable domain records`() {
        val payload = JSONObject()
            .put("snapshot_id", "snapshot")
            .put("conversation_id", "conversation")
            .put("transcript_retention", false)
            .put("messages", JSONArray().put(messagePayload()))
            .put("tasks", JSONArray().put(taskPayload()))
            .put("approvals", JSONArray().put(approvalPayload()))
            .put("voice_session", voicePayload())
        val json = eventJson("sync.snapshot", payload, id = "snapshot:snapshot", correlationId = "snapshot")

        val proto = json.toDeviceEventProto()
        val reparsed = DeviceEvent.parseFrom(proto.toByteArray())
        assertEquals(DeviceEvent.EventCase.SNAPSHOT, reparsed.eventCase)
        assertTrue(reparsed.snapshot.hasConversationId())
        assertTrue(reparsed.snapshot.hasVoiceSession())

        val snapshot = DeviceJsonCodec.decode(json) as CoreEvent.AuthoritativeSnapshot
        assertEquals("snapshot", snapshot.snapshotId)
        assertEquals("snapshot", snapshot.correlationId)
        assertEquals("conversation", snapshot.conversationId)
        assertFalse(snapshot.transcriptRetention)
        assertEquals(MessageAuthor.ASSISTANT, snapshot.messages.single().author)
        assertEquals("snapshot", snapshot.messages.single().correlationId)
        assertEquals(TaskStatus.RUNNING, snapshot.tasks.single().status)
        assertEquals("snapshot", snapshot.tasks.single().correlationId)
        assertEquals(ApprovalRisk.DESTRUCTIVE, snapshot.approvals.single().risk)
        assertEquals(ApprovalStatus.PENDING, snapshot.approvals.single().status)
        assertEquals(listOf("tool.execute"), snapshot.approvals.single().requestedScopes)
        assertFalse(snapshot.approvals.single().requestedScopes.javaClass.name.startsWith("com.google.protobuf"))
        assertEquals(DialogPhase.SPEAKING, snapshot.voiceSession?.phase)
        assertEquals(AudioRouteKind.BLUETOOTH, snapshot.voiceSession?.selectedAudioRoute)
    }

    @Test
    fun `snapshot optional fields remain absent`() {
        val payload = JSONObject()
            .put("snapshot_id", "snapshot")
            .put("conversation_id", JSONObject.NULL)
            .put("transcript_retention", true)
            .put("messages", JSONArray())
            .put("tasks", JSONArray())
            .put("approvals", JSONArray())
            .put("voice_session", JSONObject.NULL)
        val proto = eventJson("sync.snapshot", payload).toDeviceEventProto()

        assertFalse(proto.snapshot.hasConversationId())
        assertFalse(proto.snapshot.hasVoiceSession())
        val snapshot = proto.toDomain() as CoreEvent.AuthoritativeSnapshot
        assertNull(snapshot.conversationId)
        assertNull(snapshot.voiceSession)
    }

    @Test
    fun `all event discriminators map through the generated envelope`() {
        val cases = listOf(
            EventCase("command.result", commandResultPayload("COMMITTED"), CoreEvent.CommandResult::class.java),
            EventCase("conversation.message_added", messagePayload(), CoreEvent.MessageAdded::class.java),
            EventCase("task.changed", taskPayload(), CoreEvent.TaskChanged::class.java),
            EventCase("approval.changed", approvalPayload(), CoreEvent.ApprovalChanged::class.java),
            EventCase("transcript.partial", transcriptPayload(), CoreEvent.PartialTranscript::class.java),
            EventCase("transcript.final", transcriptPayload(), CoreEvent.FinalTranscript::class.java),
            EventCase("voice.incoming", voicePayload(), CoreEvent.IncomingCall::class.java),
            EventCase("voice.changed", voicePayload(), CoreEvent.VoiceChanged::class.java),
            EventCase("tts.changed", JSONObject().put("status", "SPEAKING"), CoreEvent.TtsChanged::class.java),
            EventCase(
                "tts.speak",
                JSONObject()
                    .put("session_id", "session")
                    .put("conversation_id", "conversation")
                    .put("status", "BUFFERING")
                    .put("text", "speak"),
                CoreEvent.TtsSpeak::class.java,
            ),
        )

        cases.forEach { case ->
            val json = eventJson(case.type, case.payload)
            val proto = json.toDeviceEventProto()
            assertFalse(proto.eventCase == DeviceEvent.EventCase.EVENT_NOT_SET)
            assertTrue(case.domainType.isInstance(DeviceJsonCodec.decode(json)))
        }
    }

    @Test
    fun `command result accepts only explicit terminal outcomes`() {
        val committed = DeviceJsonCodec.decode(
            eventJson("command.result", commandResultPayload("COMMITTED")),
        ) as CoreEvent.CommandResult
        val rejected = DeviceJsonCodec.decode(
            eventJson(
                "command.result",
                commandResultPayload("REJECTED").put("safe_message", "x".repeat(300)),
            ),
        ) as CoreEvent.CommandResult

        assertTrue(committed.committed)
        assertFalse(rejected.committed)
        assertEquals(240, rejected.safeMessage?.length)
        assertThrows(IllegalArgumentException::class.java) {
            DeviceJsonCodec.decode(eventJson("command.result", commandResultPayload("PENDING")))
        }
        assertThrows(IllegalArgumentException::class.java) {
            DeviceJsonCodec.decode(eventJson("command.result", commandResultPayload("committed")))
        }
        assertThrows(IllegalArgumentException::class.java) {
            DeviceJsonCodec.decode(
                eventJson(
                    "command.result",
                    commandResultPayload("COMMITTED"),
                    correlationId = "different-command",
                ),
            )
        }
    }

    @Test
    fun `event defaults and clamps stay compatible with the json edge`() {
        val message = DeviceJsonCodec.decode(
            eventJson("conversation.message_added", messagePayload().put("author", "ALIEN")),
        ) as CoreEvent.MessageAdded
        val task = DeviceJsonCodec.decode(
            eventJson(
                "task.changed",
                taskPayload().put("status", "ALIEN").put("progress_percent", 999).put("priority", -999),
            ),
        ) as CoreEvent.TaskChanged
        val approval = DeviceJsonCodec.decode(
            eventJson("approval.changed", approvalPayload().put("risk", "ALIEN").put("status", "ALIEN")),
        ) as CoreEvent.ApprovalChanged
        val voice = DeviceJsonCodec.decode(
            eventJson(
                "voice.changed",
                voicePayload()
                    .put("direction", "ALIEN")
                    .put("phase", "ALIEN")
                    .put("audio_route", "ALIEN")
                    .put("tts_status", "ALIEN"),
            ),
        ) as CoreEvent.VoiceChanged

        assertEquals(MessageAuthor.SYSTEM, message.message.author)
        assertEquals(TaskStatus.CREATED, task.task.status)
        assertEquals(100, task.task.progressPercent)
        assertEquals(-100, task.task.priority)
        assertEquals(ApprovalRisk.STATE_CHANGING, approval.approval.risk)
        assertEquals(ApprovalStatus.PENDING, approval.approval.status)
        assertEquals(CallDirection.INCOMING, voice.session.direction)
        assertEquals(DialogPhase.IDLE, voice.session.phase)
        assertEquals(AudioRouteKind.EARPIECE, voice.session.selectedAudioRoute)
        assertEquals(TtsPlaybackStatus.IDLE, voice.session.ttsPlayback)
    }

    @Test
    fun `correlation and root payload fallbacks remain compatible`() {
        val payloadAtRoot = messagePayload()
            .put("id", "event")
            .put("type", "conversation.message_added")
        val event = DeviceJsonCodec.decode(payloadAtRoot) as CoreEvent.MessageAdded

        assertEquals("event", event.correlationId)
        assertEquals("event", event.message.correlationId)
    }

    @Test
    fun `unknown event is safe while malformed known event fails closed`() {
        val unknown = DeviceJsonCodec.decode(
            eventJson("future.secret_event", JSONObject().put("secret", "do-not-reflect")),
        ) as CoreEvent.ProtocolError

        assertTrue(unknown.safeMessage.contains("future.secret_event"))
        assertFalse(unknown.safeMessage.contains("do-not-reflect"))
        assertThrows(Exception::class.java) {
            DeviceJsonCodec.decode(eventJson("task.changed", JSONObject()))
        }
    }

    @Test
    fun `tts text is byte bounded before entering generated or domain messages`() {
        val oversized = "界".repeat(MAX_TTS_TEXT_UTF8_BYTES / 3 + 10)
        val json = eventJson(
            "tts.speak",
            JSONObject()
                .put("session_id", "session")
                .put("conversation_id", "conversation")
                .put("status", "BUFFERING")
                .put("text", oversized),
        )

        val proto = json.toDeviceEventProto()
        val domain = proto.toDomain() as CoreEvent.TtsSpeak
        assertEquals(DeviceTtsStatus.DEVICE_TTS_STATUS_BUFFERING, proto.ttsSpeak.status)
        assertEquals(MAX_TTS_TEXT_UTF8_BYTES, utf8Size(proto.ttsSpeak.text))
        assertEquals(proto.ttsSpeak.text, domain.text)
    }

    private data class CommandCase(
        val command: CoreCommand,
        val type: String,
        val fields: Map<String, Any>,
        val absent: Set<String> = emptySet(),
    )

    private data class EventCase(
        val type: String,
        val payload: JSONObject,
        val domainType: Class<out CoreEvent>,
    )

    private fun eventJson(
        type: String,
        payload: JSONObject,
        id: String = "event",
        correlationId: String? = if (type == "command.result") {
            payload.optString("command_id", "correlation")
        } else {
            "correlation"
        },
    ): JSONObject = JSONObject()
        .put("id", id)
        .put("type", type)
        .put("payload", payload)
        .apply { correlationId?.let { put("correlation_id", it) } }

    private fun messagePayload(): JSONObject = JSONObject()
        .put("message_id", "message")
        .put("conversation_id", "conversation")
        .put("author", "ASSISTANT")
        .put("text", "hello")
        .put("created_at_epoch_millis", 1_700_000_000_000L)

    private fun taskPayload(): JSONObject = JSONObject()
        .put("task_id", "task")
        .put("root_task_id", "root")
        .put("conversation_id", "conversation")
        .put("goal", "goal")
        .put("assigned_agent", "agent")
        .put("status", "RUNNING")
        .put("progress_percent", 42)
        .put("summary", "summary")
        .put("created_at_epoch_millis", 1_700_000_000_000L)
        .put("updated_at_epoch_millis", 1_700_000_001_000L)
        .put("can_retry", true)
        .put("priority", 7)
        .put("dismissed", false)

    private fun approvalPayload(): JSONObject = JSONObject()
        .put("approval_id", "approval")
        .put("task_id", "task")
        .put("title", "Approve tool")
        .put("redacted_arguments", "{redacted}")
        .put("risk", "DESTRUCTIVE")
        .put("expires_at_epoch_millis", 1_700_000_010_000L)
        .put("status", "PENDING")
        .put("requested_scopes", JSONArray().put("tool.execute"))
        .put("reason", "policy")

    private fun transcriptPayload(): JSONObject = JSONObject()
        .put("conversation_id", "conversation")
        .put("text", "partial")

    private fun voicePayload(): JSONObject = JSONObject()
        .put("session_id", "session")
        .put("conversation_id", "conversation")
        .put("direction", "OUTGOING")
        .put("phase", "SPEAKING")
        .put("started_at_epoch_millis", 1_700_000_000_000L)
        .put("is_muted", false)
        .put("is_push_to_talk", true)
        .put("audio_route", "BLUETOOTH")
        .put("tts_status", "SPEAKING")
        .put("is_simulated_media", true)
        .put("media_notice", "simulated")

    private fun commandResultPayload(status: String): JSONObject = JSONObject()
        .put("command_id", "command")
        .put("command_type", "task.cancel")
        .put("status", status)
}
