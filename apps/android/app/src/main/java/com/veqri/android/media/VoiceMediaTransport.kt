package com.veqri.android.media

import com.veqri.android.data.AudioRouteKind
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow

data class MediaSessionConfig(
    val sessionId: String,
    val stunUrls: List<String> = emptyList(),
    val turnUrls: List<String> = emptyList(),
    val enableEchoCancellation: Boolean = true,
    val enableNoiseSuppression: Boolean = true,
    val enableAutomaticGainControl: Boolean = true,
)

enum class SessionDescriptionType {
    OFFER,
    ANSWER,
}

data class WebRtcSessionDescription(
    val type: SessionDescriptionType,
    val sdp: String,
)

data class WebRtcIceCandidate(
    val sdpMid: String?,
    val sdpMLineIndex: Int,
    val candidate: String,
)

sealed interface MediaTransportState {
    val isSimulated: Boolean

    data object Idle : MediaTransportState {
        override val isSimulated = false
    }

    data class Connecting(override val isSimulated: Boolean) : MediaTransportState
    data class Active(override val isSimulated: Boolean, val notice: String) : MediaTransportState
    data class Failed(override val isSimulated: Boolean, val safeMessage: String) : MediaTransportState
    data object Ended : MediaTransportState {
        override val isSimulated = false
    }
}

/** Adapter boundary compatible with native WebRTC offer/answer and ICE signaling. */
interface VoiceMediaTransport {
    val state: StateFlow<MediaTransportState>
    suspend fun start(config: MediaSessionConfig): WebRtcSessionDescription?
    suspend fun applyRemoteDescription(description: WebRtcSessionDescription)
    suspend fun addRemoteIceCandidate(candidate: WebRtcIceCandidate)
    suspend fun setMuted(isMuted: Boolean)
    suspend fun selectAudioRoute(route: AudioRouteKind)
    suspend fun interruptPlayback()
    suspend fun stop()
}

/**
 * Small surface a native WebRTC SDK adapter must implement. No SDK types leak into app state.
 */
interface WebRtcEngine {
    suspend fun createOffer(config: MediaSessionConfig): WebRtcSessionDescription
    suspend fun setRemoteDescription(description: WebRtcSessionDescription)
    suspend fun addIceCandidate(candidate: WebRtcIceCandidate)
    suspend fun setAudioEnabled(enabled: Boolean)
    suspend fun close()
}

class EngineBackedVoiceMediaTransport(
    private val engine: WebRtcEngine,
    private val audioRoutes: AudioRouteController,
) : VoiceMediaTransport {
    private val mutableState = MutableStateFlow<MediaTransportState>(MediaTransportState.Idle)
    override val state = mutableState.asStateFlow()

    override suspend fun start(config: MediaSessionConfig): WebRtcSessionDescription {
        mutableState.value = MediaTransportState.Connecting(isSimulated = false)
        audioRoutes.start()
        return runCatching { engine.createOffer(config) }
            .onSuccess {
                mutableState.value = MediaTransportState.Active(
                    isSimulated = false,
                    notice = "Native WebRTC media is active.",
                )
            }
            .onFailure {
                audioRoutes.stop()
                mutableState.value = MediaTransportState.Failed(
                    isSimulated = false,
                    safeMessage = "The encrypted media session could not start.",
                )
            }
            .getOrThrow()
    }

    override suspend fun applyRemoteDescription(description: WebRtcSessionDescription) {
        engine.setRemoteDescription(description)
    }

    override suspend fun addRemoteIceCandidate(candidate: WebRtcIceCandidate) {
        engine.addIceCandidate(candidate)
    }

    override suspend fun setMuted(isMuted: Boolean) {
        engine.setAudioEnabled(!isMuted)
    }

    override suspend fun selectAudioRoute(route: AudioRouteKind) {
        check(audioRoutes.select(route)) { "The selected audio route is not currently available." }
    }

    override suspend fun interruptPlayback() {
        // TTS playback belongs to its provider; CoreCommand.InterruptTts carries the durable control.
    }

    override suspend fun stop() {
        engine.close()
        audioRoutes.stop()
        mutableState.value = MediaTransportState.Ended
    }
}

class SimulatedVoiceMediaTransport(
    private val audioRoutes: AudioRouteController? = null,
) : VoiceMediaTransport {
    private val mutableState = MutableStateFlow<MediaTransportState>(MediaTransportState.Idle)
    private var muted = false
    private var interrupted = false
    override val state = mutableState.asStateFlow()

    override suspend fun start(config: MediaSessionConfig): WebRtcSessionDescription? {
        mutableState.value = MediaTransportState.Connecting(isSimulated = true)
        audioRoutes?.start()
        mutableState.value = MediaTransportState.Active(
            isSimulated = true,
            notice = SIMULATOR_NOTICE,
        )
        return null
    }

    override suspend fun applyRemoteDescription(description: WebRtcSessionDescription) = Unit
    override suspend fun addRemoteIceCandidate(candidate: WebRtcIceCandidate) = Unit

    override suspend fun setMuted(isMuted: Boolean) {
        muted = isMuted
    }

    override suspend fun selectAudioRoute(route: AudioRouteKind) {
        val controller = audioRoutes ?: return
        check(controller.select(route)) { "The selected audio route is not available." }
    }

    override suspend fun interruptPlayback() {
        interrupted = true
    }

    override suspend fun stop() {
        audioRoutes?.stop()
        mutableState.value = MediaTransportState.Ended
    }

    fun isMutedForTest(): Boolean = muted
    fun wasInterruptedForTest(): Boolean = interrupted

    companion object {
        const val SIMULATOR_NOTICE =
            "Simulated local media: call state, transcript, routing, and interruption are exercised; no audio packets are sent."
    }
}

class UnavailableVoiceMediaTransport : VoiceMediaTransport {
    private val mutableState = MutableStateFlow<MediaTransportState>(MediaTransportState.Idle)
    override val state = mutableState.asStateFlow()

    override suspend fun start(config: MediaSessionConfig): WebRtcSessionDescription? {
        val message = "No native WebRTC engine is packaged in this build."
        mutableState.value = MediaTransportState.Failed(isSimulated = false, safeMessage = message)
        throw MediaUnavailableException(message)
    }

    override suspend fun applyRemoteDescription(description: WebRtcSessionDescription) = unavailable()
    override suspend fun addRemoteIceCandidate(candidate: WebRtcIceCandidate) = unavailable()
    override suspend fun setMuted(isMuted: Boolean) = unavailable()
    override suspend fun selectAudioRoute(route: AudioRouteKind) = unavailable()
    override suspend fun interruptPlayback() = unavailable()
    override suspend fun stop() {
        mutableState.value = MediaTransportState.Ended
    }

    private fun unavailable(): Nothing =
        throw MediaUnavailableException("No native WebRTC engine is packaged in this build.")
}

class MediaUnavailableException(message: String) : IllegalStateException(message)
