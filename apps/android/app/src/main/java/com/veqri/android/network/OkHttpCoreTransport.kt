package com.veqri.android.network

import com.veqri.android.BuildConfig
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
import java.net.URI
import java.time.Instant
import java.util.Locale
import java.util.UUID
import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicBoolean
import java.util.concurrent.atomic.AtomicInteger
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineDispatcher
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.channels.Channel
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.flow
import kotlinx.coroutines.launch
import kotlinx.coroutines.withTimeout
import kotlinx.coroutines.TimeoutCancellationException
import kotlinx.coroutines.withContext
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import okhttp3.Response
import okhttp3.WebSocket
import okhttp3.WebSocketListener
import org.json.JSONObject

private data class QueuedCoreEvent(
	val event: CoreEvent,
	val socketGeneration: Int,
)

internal fun shouldDeliverSocketEvent(eventGeneration: Int, activeGeneration: Int): Boolean =
	eventGeneration == 0 || eventGeneration == activeGeneration

/**
 * Authenticated HTTP pairing plus authenticated WebSocket event/command transport.
 * It owns reconnect attempts but never retries a command, because commands may have side effects.
 */
class OkHttpCoreTransport(
    private val scope: CoroutineScope,
    private val ioDispatcher: CoroutineDispatcher = Dispatchers.IO,
    private val client: OkHttpClient = defaultClient(),
) : CoreTransport {
    private val mutableConnection = MutableStateFlow<ConnectionStatus>(ConnectionStatus.Offline)
    // WebSocket callbacks cannot suspend. Durable catch-up occupies one slot as
    // an authoritative snapshot; live deltas retain a hard memory bound.
    private val eventChannel = Channel<QueuedCoreEvent>(capacity = EVENT_QUEUE_CAPACITY)
    private val queuedEventCount = AtomicInteger()
    private val waitForBacklogDrain = AtomicBoolean()
    private val pendingCommandCommits =
        ConcurrentHashMap<String, CompletableDeferred<CoreEvent.CommandResult>>()
    private val socketGeneration = AtomicInteger()
    @Volatile
    private var armedRotationCandidate: CredentialRotationCandidate? = null
    @Volatile
    private var activeCredential: DeviceCredential? = null
    @Volatile
    private var socket: WebSocket? = null
    @Volatile
    private var requestedDisconnect = false
    @Volatile
    private var reconnectAttempt = 0
    @Volatile
    private var reconnectJob: Job? = null
    @Volatile
    private var awaitingAuthoritativeSnapshot = false

    override val connection = mutableConnection.asStateFlow()
    override val events: Flow<CoreEvent> = flow {
        for (queued in eventChannel) {
            try {
				if (!shouldDeliverSocketEvent(queued.socketGeneration, socketGeneration.get())) {
					continue
				}
				val event = queued.event
				if (event is CoreEvent.CommandResult) {
					pendingCommandCommits.remove(event.commandId)?.complete(event)
				} else {
					emit(event)
					if (event is CoreEvent.AuthoritativeSnapshot && socket != null &&
						queued.socketGeneration == socketGeneration.get()
					) {
						awaitingAuthoritativeSnapshot = false
						reconnectAttempt = 0
						reconnectJob = null
						mutableConnection.value = ConnectionStatus.Connected(CoreTransport.PROTOCOL_VERSION)
					}
				}
            } finally {
                queuedEventCount.decrementAndGet()
            }
        }
    }

    override suspend fun pair(request: PairingRequest): Result<DeviceCredential> = withContext(ioDispatcher) {
        runCatching {
            val baseUrl = EndpointPolicy.requireAllowedBaseUrl(request.coreBaseUrl)
            val body = JSONObject()
                .put("one_time_code", request.oneTimeCode)
                .put("device_name", request.deviceName)
                .put("retain_transcript", request.retainTranscript)
                .put("client_protocol_version", request.clientProtocolVersion)
                .toString()
                .toRequestBody(JSON_MEDIA_TYPE)
            val httpRequest = Request.Builder()
                .url("$baseUrl/v1/pairing/claim")
                .header("Accept", "application/json")
                .header("X-Veqri-Protocol-Version", request.clientProtocolVersion.toString())
                .post(body)
                .build()
            client.newCall(httpRequest).execute().use { response ->
                if (!response.isSuccessful) {
                    throw PairingException(pairingFailureMessage(response.code))
                }
                val payload = JSONObject(response.body.string())
                val negotiated = payload.optInt("protocol_version", CoreTransport.PROTOCOL_VERSION)
                if (negotiated != CoreTransport.PROTOCOL_VERSION) {
                    throw PairingException("The core selected an unsupported protocol version.")
                }
                DeviceCredential(
                    deviceId = payload.getString("device_id"),
                    accessToken = payload.getString("access_token"),
                    coreBaseUrl = baseUrl,
                    issuedAtEpochMillis = payload.optLong("issued_at_epoch_millis", System.currentTimeMillis()),
                    keyVersion = payload.optJSONObject("device")?.optInt("key_version", 1) ?: 1,
                )
            }
        }
    }

    override suspend fun prepareCredentialRotation(
        active: DeviceCredential,
    ): CredentialRotationCandidate = withContext(ioDispatcher) {
        try {
            val baseUrl = EndpointPolicy.requireAllowedBaseUrl(active.coreBaseUrl)
            require(active.deviceId.isNotBlank() && active.accessToken.isNotBlank())
            val request = rotationRequest(
                baseUrl = baseUrl,
                path = "/v1/devices/self/credential-rotation/prepare",
                credential = active.accessToken,
                body = ByteArray(0).toRequestBody(null),
            )
            client.newCall(request).execute().use { response ->
                when (response.code) {
                    201 -> Unit
                    401, 403 -> throw CredentialRotationException(
                        CredentialRotationFailureKind.UNAUTHORIZED,
                        "The active device credential is no longer authorized.",
                    )
                    409 -> throw CredentialRotationException(
                        CredentialRotationFailureKind.PENDING,
                        "Core already has a pending credential rotation.",
                    )
                    else -> throw CredentialRotationException(
                        CredentialRotationFailureKind.TRANSIENT,
                        "Core could not prepare a replacement credential.",
                    )
                }
                val payload = JSONObject(response.body.string())
                requireRotationProtocol(payload)
                val candidate = CredentialRotationCandidate(
                    deviceId = payload.getString("device_id"),
                    accessToken = payload.getString("credential"),
                    coreBaseUrl = baseUrl,
                    keyVersion = payload.getInt("key_version"),
                    preparedAtEpochMillis = payload.instantMillis("prepared_at"),
                    expiresAtEpochMillis = payload.instantMillis("expires_at"),
                    correlationId = payload.getString("correlation_id"),
                )
                if (candidate.deviceId != active.deviceId || candidate.accessToken.isBlank() ||
                    candidate.keyVersion <= active.keyVersion ||
                    candidate.expiresAtEpochMillis <= candidate.preparedAtEpochMillis
                ) {
                    throw CredentialRotationException(
                        CredentialRotationFailureKind.INVALID_RESPONSE,
                        "Core returned invalid credential rotation metadata.",
                    )
                }
                candidate
            }
        } catch (error: CredentialRotationException) {
            throw error
        } catch (error: Exception) {
            throw CredentialRotationException(
                CredentialRotationFailureKind.TRANSIENT,
                "Core could not be reached to prepare credential rotation.",
                error,
            )
        }
    }

    override suspend fun armCredentialRotation(candidate: CredentialRotationCandidate?) {
        if (candidate == null) {
            armedRotationCandidate = null
            return
        }
        val normalized = candidate.copy(
            coreBaseUrl = EndpointPolicy.requireAllowedBaseUrl(candidate.coreBaseUrl),
        )
        require(normalized.deviceId.isNotBlank() && normalized.accessToken.isNotBlank() && normalized.keyVersion >= 2)
        activeCredential?.let { active ->
            require(active.deviceId == normalized.deviceId && active.coreBaseUrl == normalized.coreBaseUrl)
        }
        armedRotationCandidate = normalized
    }

    override suspend fun confirmCredentialRotation(
        candidate: CredentialRotationCandidate,
    ): CredentialRotationConfirmation = withContext(ioDispatcher) {
        try {
            val baseUrl = EndpointPolicy.requireAllowedBaseUrl(candidate.coreBaseUrl)
            val body = JSONObject()
                .put("key_version", candidate.keyVersion)
                .toString()
                .toRequestBody(JSON_MEDIA_TYPE)
            val request = rotationRequest(
                baseUrl = baseUrl,
                path = "/v1/devices/self/credential-rotation/confirm",
                credential = candidate.accessToken,
                body = body,
            )
            client.newCall(request).execute().use { response ->
                when (response.code) {
                    200 -> Unit
                    410 -> throw CredentialRotationException(
                        CredentialRotationFailureKind.EXPIRED,
                        "The prepared credential expired before confirmation.",
                    )
                    401, 403 -> throw CredentialRotationException(
                        CredentialRotationFailureKind.UNAUTHORIZED,
                        "The prepared credential was not accepted by Core.",
                    )
                    else -> throw CredentialRotationException(
                        CredentialRotationFailureKind.TRANSIENT,
                        "Core could not confirm credential rotation.",
                    )
                }
                val payload = JSONObject(response.body.string())
                requireRotationProtocol(payload)
                val confirmation = CredentialRotationConfirmation(
                    deviceId = payload.getString("device_id"),
                    keyVersion = payload.getInt("key_version"),
                    alreadyConfirmed = payload.optBoolean("already_confirmed", false),
                    correlationId = payload.getString("correlation_id"),
                )
                if (!payload.optBoolean("confirmed", false) || confirmation.deviceId != candidate.deviceId ||
                    confirmation.keyVersion != candidate.keyVersion
                ) {
                    throw CredentialRotationException(
                        CredentialRotationFailureKind.INVALID_RESPONSE,
                        "Core returned invalid credential confirmation metadata.",
                    )
                }
                confirmation
            }
        } catch (error: CredentialRotationException) {
            throw error
        } catch (error: Exception) {
            throw CredentialRotationException(
                CredentialRotationFailureKind.TRANSIENT,
                "Credential confirmation did not complete; the stored replacement was preserved.",
                error,
            )
        }
    }

    override suspend fun cancelCredentialRotation(active: DeviceCredential): Boolean = withContext(ioDispatcher) {
        try {
            val baseUrl = EndpointPolicy.requireAllowedBaseUrl(active.coreBaseUrl)
            val request = rotationRequest(
                baseUrl = baseUrl,
                path = "/v1/devices/self/credential-rotation/cancel",
                credential = active.accessToken,
                body = ByteArray(0).toRequestBody(null),
            )
            client.newCall(request).execute().use { response ->
                when (response.code) {
                    200 -> Unit
                    401, 403 -> throw CredentialRotationException(
                        CredentialRotationFailureKind.UNAUTHORIZED,
                        "The active device credential was not accepted by Core.",
                    )
                    else -> throw CredentialRotationException(
                        CredentialRotationFailureKind.TRANSIENT,
                        "Core could not cancel the pending credential rotation.",
                    )
                }
                val payload = JSONObject(response.body.string())
                requireRotationProtocol(payload)
                if (payload.optString("device_id") != active.deviceId) {
                    throw CredentialRotationException(
                        CredentialRotationFailureKind.INVALID_RESPONSE,
                        "Core returned invalid credential cancellation metadata.",
                    )
                }
                payload.optBoolean("cancelled", false)
            }
        } catch (error: CredentialRotationException) {
            throw error
        } catch (error: Exception) {
            throw CredentialRotationException(
                CredentialRotationFailureKind.TRANSIENT,
                "Core could not be reached to cancel credential rotation.",
                error,
            )
        }
    }

    override suspend fun connect(credential: DeviceCredential) {
        val validatedCredential = credential.copy(
            coreBaseUrl = EndpointPolicy.requireAllowedBaseUrl(credential.coreBaseUrl),
        )
        requestedDisconnect = false
        reconnectJob?.cancel()
        reconnectAttempt = 0
        if (activeCredential == validatedCredential && socket != null) {
            if (armedRotationCandidate?.keyVersion == validatedCredential.keyVersion) {
                armedRotationCandidate = null
            }
            return
        }
        val previousSocket = socket
        socket = null
        if (previousSocket != null) {
            failPendingCommandCommits("The Core connection changed before command confirmation.")
        }
        previousSocket?.close(NORMAL_CLOSURE, "credential handoff")
        activeCredential = validatedCredential
        if (armedRotationCandidate?.keyVersion == validatedCredential.keyVersion) {
            armedRotationCandidate = null
        }
        openSocket(validatedCredential, isReconnect = false)
    }

    override suspend fun send(command: CoreCommand) {
        val webSocket = socket ?: throw TransportUnavailableException("Veqri Core is not connected.")
        val accepted = webSocket.send(command.toJson().toString())
        if (!accepted) {
            throw TransportUnavailableException("The command could not be queued for delivery.")
        }
    }

    override suspend fun sendAwaitingCommit(command: CoreCommand): CommandCommitResult {
        val payload = command.toJson()
        val commandType = payload.getString("type")
        val pending = CompletableDeferred<CoreEvent.CommandResult>()
        check(pendingCommandCommits.putIfAbsent(command.commandId, pending) == null) {
            "A command with this identifier is already awaiting confirmation."
        }
        val webSocket = socket
        if (webSocket == null) {
            pendingCommandCommits.remove(command.commandId, pending)
            throw TransportUnavailableException("Veqri Core is not connected.")
        }
        if (!webSocket.send(payload.toString())) {
            pendingCommandCommits.remove(command.commandId, pending)
            throw TransportUnavailableException("The command could not be queued for delivery.")
        }
        try {
            val result = withTimeout(COMMAND_COMMIT_TIMEOUT_MILLIS) { pending.await() }
            if (result.commandType != commandType) {
                throw CommandCommitException(
                    CommandCommitFailureKind.OUTCOME_UNKNOWN,
                    "Core returned mismatched command confirmation metadata.",
                )
            }
            if (!result.committed) {
                throw CommandCommitException(
                    CommandCommitFailureKind.REJECTED,
                    result.safeMessage ?: "Core rejected the privacy preference.",
                )
            }
            return CommandCommitResult(result.commandId, result.commandType)
        } catch (error: TimeoutCancellationException) {
			webSocket.close(COMMAND_CONFIRMATION_CLOSURE, "command confirmation timeout")
            throw CommandCommitException(
                CommandCommitFailureKind.OUTCOME_UNKNOWN,
                "Core did not confirm whether the privacy preference committed. Reconnecting will reconcile it.",
                error,
            )
		} catch (error: CommandCommitException) {
			if (error.kind == CommandCommitFailureKind.OUTCOME_UNKNOWN) {
				webSocket.close(COMMAND_CONFIRMATION_CLOSURE, "command outcome unknown")
			}
			throw error
		} catch (error: CancellationException) {
			webSocket.close(COMMAND_CONFIRMATION_CLOSURE, "command waiter cancelled")
			throw error
        } finally {
            pendingCommandCommits.remove(command.commandId, pending)
        }
    }

	override fun isAwaitingAuthoritativeSnapshot(): Boolean = awaitingAuthoritativeSnapshot

    override suspend fun disconnect() {
        requestedDisconnect = true
        reconnectJob?.cancel()
        reconnectJob = null
        socket?.close(NORMAL_CLOSURE, "client disconnect")
        socket = null
        activeCredential = null
        armedRotationCandidate = null
        failPendingCommandCommits("The Core connection closed before command confirmation.")
        mutableConnection.value = ConnectionStatus.Offline
    }

    private fun openSocket(credential: DeviceCredential, isReconnect: Boolean) {
		awaitingAuthoritativeSnapshot = true
		val generation = socketGeneration.incrementAndGet()
        mutableConnection.value = if (isReconnect) {
            ConnectionStatus.Reconnecting(reconnectAttempt, retryInMillis = 0)
        } else {
            ConnectionStatus.Connecting
        }
        val webSocketUrl = credential.coreBaseUrl
            .replaceFirst("https://", "wss://")
            .replaceFirst("http://", "ws://") + "/v1/device/events"
        val request = Request.Builder()
            .url(webSocketUrl)
            .header("Authorization", "Bearer ${credential.accessToken}")
            .header("X-Veqri-Device-Id", credential.deviceId)
            .header("X-Veqri-Protocol-Version", CoreTransport.PROTOCOL_VERSION.toString())
            .header("Sec-WebSocket-Protocol", "veqri.v1")
            .build()
        socket = client.newWebSocket(request, SocketListener(generation))
    }

    private fun scheduleReconnect() {
        if (requestedDisconnect || reconnectJob?.isActive == true) return
        val credential = activeCredential ?: return
        reconnectAttempt += 1
        val waitMillis = reconnectDelayMillis(reconnectAttempt)
        mutableConnection.value = ConnectionStatus.Reconnecting(reconnectAttempt, waitMillis)
        val drainBacklog = waitForBacklogDrain.getAndSet(false)
        reconnectJob = scope.launch {
            if (drainBacklog) {
                while (!requestedDisconnect && queuedEventCount.get() > EVENT_QUEUE_LOW_WATER) {
                    delay(BACKLOG_POLL_MILLIS)
                }
            }
            delay(waitMillis)
            reconnectJob = null
            if (!requestedDisconnect && socket == null) openSocket(credential, isReconnect = true)
        }
    }

    private inner class SocketListener(private val generation: Int) : WebSocketListener() {
        private var acceptingMessages = true

        override fun onOpen(webSocket: WebSocket, response: Response) {
            if (requestedDisconnect || socket !== webSocket) {
                webSocket.close(NORMAL_CLOSURE, "stale connection")
                return
            }
            // Core's first frame is the authoritative snapshot. Do not expose a
            // usable Connected state until the repository has applied it.
        }

        override fun onMessage(webSocket: WebSocket, text: String) {
            if (!acceptingMessages || socket !== webSocket) return
            runCatching { parseEvent(JSONObject(text)) }
                .onSuccess { event -> enqueue(webSocket, event, generation) }
                .onFailure {
                    enqueue(
                        webSocket,
                        CoreEvent.ProtocolError(
                            eventId = UUID.randomUUID().toString(),
                            correlationId = "protocol",
                            safeMessage = "Veqri received an event it could not understand.",
                        ),
                        generation,
                    )
                }
        }

        private fun enqueue(webSocket: WebSocket, event: CoreEvent, generation: Int) {
            queuedEventCount.incrementAndGet()
            if (eventChannel.trySend(QueuedCoreEvent(event, generation)).isSuccess) return
            queuedEventCount.decrementAndGet()
            acceptingMessages = false
            waitForBacklogDrain.set(true)
            webSocket.close(BACKLOG_CLOSURE, "client event backlog")
        }

        override fun onClosing(webSocket: WebSocket, code: Int, reason: String) {
            webSocket.close(code, null)
        }

        override fun onClosed(webSocket: WebSocket, code: Int, reason: String) {
            if (socket !== webSocket) return
            socket = null
            socketGeneration.compareAndSet(generation, generation + 1)
            failPendingCommandCommits("The Core connection closed before command confirmation.")
            if (code == CREDENTIAL_ROTATED_CLOSE) {
                val candidate = armedRotationCandidate
                if (candidate == null) {
                    mutableConnection.value = ConnectionStatus.Failed(
                        "Core rotated this device credential, but no stored replacement is available. Pair again.",
                    )
                    return
                }
                activeCredential = candidate.toPromotedCredential()
                scope.launch {
                    queuedEventCount.incrementAndGet()
                    try {
                        eventChannel.send(
							QueuedCoreEvent(
								CoreEvent.CredentialRotationCommitted(
									eventId = "credential-rotation:${candidate.correlationId}",
									correlationId = candidate.correlationId,
									keyVersion = candidate.keyVersion,
								),
								socketGeneration = GLOBAL_EVENT_GENERATION,
							),
                        )
                    } catch (error: Exception) {
                        queuedEventCount.decrementAndGet()
                    }
                }
                if (!requestedDisconnect) scheduleReconnect()
            } else if (code in AUTHENTICATION_CLOSE_CODES) {
                mutableConnection.value = ConnectionStatus.Failed(
                    "This device was revoked or its credential expired. Pair it again.",
                )
            } else if (!requestedDisconnect) {
                scheduleReconnect()
            }
        }

        override fun onFailure(webSocket: WebSocket, t: Throwable, response: Response?) {
            if (socket !== webSocket) return
            socket = null
            socketGeneration.compareAndSet(generation, generation + 1)
            failPendingCommandCommits("The Core connection failed before command confirmation.", t)
            if (response?.code in setOf(401, 403)) {
                mutableConnection.value = ConnectionStatus.Failed(
                    "This device is no longer authorized. Pair it again.",
                )
            } else if (!requestedDisconnect) {
                scheduleReconnect()
            }
        }
    }

    private fun failPendingCommandCommits(message: String, cause: Throwable? = null) {
        pendingCommandCommits.forEach { (commandId, pending) ->
            if (pendingCommandCommits.remove(commandId, pending)) {
                pending.completeExceptionally(
                    CommandCommitException(CommandCommitFailureKind.OUTCOME_UNKNOWN, message, cause),
                )
            }
        }
    }

    companion object {
        private const val NORMAL_CLOSURE = 1000
        private const val COMMAND_COMMIT_TIMEOUT_MILLIS = 10_000L
        private const val COMMAND_CONFIRMATION_CLOSURE = 4005
		private const val GLOBAL_EVENT_GENERATION = 0
        private const val BACKLOG_CLOSURE = 1013
        private const val CREDENTIAL_ROTATED_CLOSE = 4004
        private const val EVENT_QUEUE_CAPACITY = 1_024
        private const val EVENT_QUEUE_LOW_WATER = EVENT_QUEUE_CAPACITY / 2
        private const val BACKLOG_POLL_MILLIS = 50L
        private val AUTHENTICATION_CLOSE_CODES = setOf(4_001, 4_003)
        private val JSON_MEDIA_TYPE = "application/json; charset=utf-8".toMediaType()

        fun defaultClient(): OkHttpClient = OkHttpClient.Builder()
            .connectTimeout(10, TimeUnit.SECONDS)
            .readTimeout(15, TimeUnit.SECONDS)
            .writeTimeout(10, TimeUnit.SECONDS)
            .pingInterval(20, TimeUnit.SECONDS)
            .retryOnConnectionFailure(false)
            .build()

        private fun reconnectDelayMillis(attempt: Int): Long = when (attempt) {
            1 -> 500
            2 -> 1_000
            3 -> 2_000
            4 -> 4_000
            else -> 8_000
        }

        private fun pairingFailureMessage(statusCode: Int): String = when (statusCode) {
            400 -> "The pairing request was not accepted. Check the code and endpoint."
            401, 403 -> "The one-time pairing code is invalid or expired."
            409 -> "That one-time pairing code has already been used."
            426 -> "Update Veqri Android or Veqri Core before pairing."
            else -> "Pairing failed because Veqri Core returned HTTP $statusCode."
        }
    }
}

private fun OkHttpCoreTransport.rotationRequest(
    baseUrl: String,
    path: String,
    credential: String,
    body: okhttp3.RequestBody,
): Request = Request.Builder()
    .url(baseUrl + path)
    .header("Accept", "application/json")
    .header("Authorization", "Bearer $credential")
    .header("X-Veqri-Protocol-Version", CoreTransport.PROTOCOL_VERSION.toString())
    .post(body)
    .build()

private fun requireRotationProtocol(payload: JSONObject) {
    if (payload.optInt("protocol_version", CoreTransport.PROTOCOL_VERSION) != CoreTransport.PROTOCOL_VERSION) {
        throw CredentialRotationException(
            CredentialRotationFailureKind.INVALID_RESPONSE,
            "Core returned an unsupported credential rotation protocol version.",
        )
    }
}

private fun JSONObject.instantMillis(name: String): Long = try {
    Instant.parse(getString(name)).toEpochMilli()
} catch (error: Exception) {
    throw CredentialRotationException(
        CredentialRotationFailureKind.INVALID_RESPONSE,
        "Core returned an invalid credential rotation timestamp.",
        error,
    )
}

object EndpointPolicy {
    fun requireAllowedBaseUrl(rawUrl: String, debugBuild: Boolean = BuildConfig.DEBUG): String {
        val normalized = rawUrl.trim().trimEnd('/')
        val uri = runCatching { URI(normalized) }
            .getOrElse { throw IllegalArgumentException("Enter a valid Veqri Core URL.") }
        require(uri.userInfo == null) { "Credentials must not be embedded in the Core URL." }
        require(!uri.host.isNullOrBlank()) { "The Core URL must include a host." }
        val scheme = uri.scheme?.lowercase(Locale.US)
        val isDevelopmentHost = uri.host.equals("localhost", ignoreCase = true) || uri.host == "10.0.2.2"
        require(scheme == "https" || (scheme == "http" && debugBuild && isDevelopmentHost)) {
            "TLS is required except for emulator or same-device localhost development."
        }
        require(uri.rawQuery == null && uri.rawFragment == null) {
            "The Core URL cannot contain a query or fragment."
        }
        return normalized
    }
}

private fun CoreCommand.toJson(): JSONObject {
    val payload = JSONObject()
        .put("command_id", commandId)
        .put("protocol_version", CoreTransport.PROTOCOL_VERSION)
    return when (this) {
        is CoreCommand.SendText -> payload
            .put("type", "conversation.send_text")
            .put("conversation_id", conversationId)
            .put("text", text)
			.put("retain_transcript", retainTranscript)
		is CoreCommand.SetTranscriptRetention -> payload
			.put("type", "conversation.set_transcript_retention")
			.put("conversation_id", conversationId)
			.put("enabled", enabled)
		is CoreCommand.StartCall -> payload.put("type", "voice.start").put("retain_transcript", retainTranscript)
		is CoreCommand.SimulateIncomingCall -> payload.put("type", "debug.voice.incoming").put("retain_transcript", retainTranscript)
        is CoreCommand.AnswerCall -> payload.put("type", "voice.answer").put("session_id", sessionId)
        is CoreCommand.DeclineCall -> payload.put("type", "voice.decline").put("session_id", sessionId)
        is CoreCommand.EndCall -> payload.put("type", "voice.end").put("session_id", sessionId)
        is CoreCommand.SetMuted -> payload
            .put("type", "voice.set_muted")
            .put("session_id", sessionId)
            .put("is_muted", isMuted)
        is CoreCommand.SetPushToTalk -> payload
            .put("type", "voice.set_push_to_talk")
            .put("session_id", sessionId)
            .put("enabled", enabled)
        is CoreCommand.SelectAudioRoute -> payload
            .put("type", "voice.select_audio_route")
            .put("session_id", sessionId)
            .put("route", route.name)
        is CoreCommand.InterruptTts -> payload
            .put("type", "voice.interrupt_tts")
            .put("session_id", sessionId)
        is CoreCommand.ResolveApproval -> payload
            .put("type", if (approved) "approval.approve" else "approval.deny")
            .put("approval_id", approvalId)
        is CoreCommand.CancelTask -> payload.put("type", "task.cancel").put("task_id", taskId)
        is CoreCommand.RetryTask -> payload.put("type", "task.retry").put("task_id", taskId)
    }
}

internal fun parseEvent(json: JSONObject): CoreEvent {
    val type = json.getString("type")
    val eventId = json.getString("id")
    val correlationId = json.optString("correlation_id", eventId)
    val payload = json.optJSONObject("payload") ?: json
    return when (type) {
        "sync.snapshot" -> CoreEvent.AuthoritativeSnapshot(
            eventId = eventId,
            correlationId = correlationId,
            snapshotId = payload.getString("snapshot_id"),
            conversationId = payload.optionalString("conversation_id"),
            transcriptRetention = payload.getBoolean("transcript_retention"),
            messages = payload.objectList("messages") { item ->
                item.toConversationMessage(item.optString("correlation_id", correlationId))
            },
            tasks = payload.objectList("tasks") { item ->
                item.toTaskRecord(item.optString("correlation_id", correlationId))
            },
            approvals = payload.objectList("approvals", JSONObject::toApprovalRequest),
            voiceSession = payload.optJSONObject("voice_session")?.toVoiceSession(),
        )
        "command.result" -> CoreEvent.CommandResult(
            eventId = eventId,
            correlationId = correlationId,
            commandId = payload.getString("command_id"),
            commandType = payload.getString("command_type"),
            committed = commandResultCommitted(payload.getString("status")),
            safeMessage = payload.optionalString("safe_message")?.take(240),
        )
        "conversation.message_added" -> CoreEvent.MessageAdded(
            eventId,
            correlationId,
            payload.toConversationMessage(correlationId),
        )
        "task.changed" -> CoreEvent.TaskChanged(
            eventId,
            correlationId,
            payload.toTaskRecord(correlationId),
        )
        "approval.changed" -> CoreEvent.ApprovalChanged(
            eventId,
            correlationId,
            payload.toApprovalRequest(),
        )
        "transcript.partial" -> CoreEvent.PartialTranscript(
            eventId,
            correlationId,
            payload.getString("conversation_id"),
            payload.optString("text"),
        )
        "transcript.final" -> CoreEvent.FinalTranscript(
            eventId,
            correlationId,
            payload.getString("conversation_id"),
            payload.optString("text"),
        )
        "voice.incoming" -> CoreEvent.IncomingCall(eventId, correlationId, payload.toVoiceSession())
        "voice.changed" -> CoreEvent.VoiceChanged(eventId, correlationId, payload.toVoiceSession())
        "tts.changed" -> CoreEvent.TtsChanged(
            eventId,
            correlationId,
            enumValue(payload.optString("status"), TtsPlaybackStatus.IDLE),
        )
        "tts.speak" -> CoreEvent.TtsSpeak(
            eventId,
            correlationId,
            payload.getString("session_id"),
            payload.getString("conversation_id"),
            boundTtsText(payload.getString("text")),
        )
        else -> CoreEvent.ProtocolError(eventId, correlationId, "Unsupported event type: $type")
    }
}

private fun JSONObject.toConversationMessage(defaultCorrelationId: String) = ConversationMessage(
    id = getString("message_id"),
    conversationId = getString("conversation_id"),
    author = enumValue(optString("author"), MessageAuthor.SYSTEM),
    text = getString("text"),
    createdAtEpochMillis = optLong("created_at_epoch_millis", System.currentTimeMillis()),
    correlationId = optString("correlation_id", defaultCorrelationId),
)

private fun JSONObject.toTaskRecord(defaultCorrelationId: String): TaskRecord {
    val taskId = getString("task_id")
    return TaskRecord(
        id = taskId,
        rootTaskId = optString("root_task_id").ifBlank { taskId },
        conversationId = optString("conversation_id"),
        goal = optString("goal"),
        assignedAgent = optString("assigned_agent", "unassigned"),
        status = enumValue(optString("status"), TaskStatus.CREATED),
        progressPercent = optInt("progress_percent", 0).coerceIn(0, 100),
        summary = optString("summary"),
        createdAtEpochMillis = optLong("created_at_epoch_millis"),
        updatedAtEpochMillis = optLong("updated_at_epoch_millis"),
        correlationId = optString("correlation_id", defaultCorrelationId),
        canRetry = optBoolean("can_retry", false),
        priority = optInt("priority", 0).coerceIn(-100, 100),
        dismissed = optBoolean("dismissed", false),
    )
}

private fun JSONObject.toApprovalRequest() = ApprovalRequest(
    id = getString("approval_id"),
    taskId = getString("task_id"),
    title = optString("title", "Tool approval required"),
    redactedArguments = optString("redacted_arguments", "Arguments hidden"),
    risk = enumValue(optString("risk"), ApprovalRisk.STATE_CHANGING),
    expiresAtEpochMillis = optLong("expires_at_epoch_millis"),
    status = enumValue(optString("status"), ApprovalStatus.PENDING),
    requestedScopes = stringList("requested_scopes"),
    reason = optString("reason"),
)

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

internal fun boundTtsText(text: String): String {
    if (text.length <= CoreTransport.MAX_TTS_TEXT_CHARS) return text
    var end = CoreTransport.MAX_TTS_TEXT_CHARS
    if (Character.isHighSurrogate(text[end - 1]) && Character.isLowSurrogate(text[end])) {
        end--
    }
    return text.substring(0, end)
}

internal fun commandResultCommitted(status: String): Boolean = when (status) {
    "COMMITTED" -> true
    "REJECTED" -> false
    // Anything else is an unknown outcome; parsing must fail so the commit waiter times out and reconnects.
    else -> throw IllegalArgumentException("Unsupported command result status.")
}

private fun JSONObject.optionalString(name: String): String? =
    if (!has(name) || isNull(name)) null else getString(name).takeIf(String::isNotBlank)

private fun JSONObject.toVoiceSession() = VoiceSession(
    id = getString("session_id"),
    conversationId = getString("conversation_id"),
    direction = enumValue(optString("direction"), CallDirection.INCOMING),
    phase = enumValue(optString("phase"), DialogPhase.IDLE),
    startedAtEpochMillis = optLong("started_at_epoch_millis").takeIf { it > 0 },
    isMuted = optBoolean("is_muted"),
    isPushToTalk = optBoolean("is_push_to_talk"),
    selectedAudioRoute = enumValue(optString("audio_route"), AudioRouteKind.EARPIECE),
    ttsPlayback = enumValue(optString("tts_status"), TtsPlaybackStatus.IDLE),
    isSimulatedMedia = optBoolean("is_simulated_media", false),
    mediaNotice = optString("media_notice"),
)

private inline fun <reified T : Enum<T>> enumValue(value: String, fallback: T): T =
    enumValues<T>().firstOrNull { it.name.equals(value, ignoreCase = true) } ?: fallback

class TransportUnavailableException(message: String) : IllegalStateException(message)
