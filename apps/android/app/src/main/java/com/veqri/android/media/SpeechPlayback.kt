package com.veqri.android.media

import android.content.Context
import android.speech.tts.TextToSpeech
import android.speech.tts.UtteranceProgressListener
import com.veqri.android.data.TtsPlaybackStatus
import java.util.Locale
import java.util.concurrent.atomic.AtomicLong
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.coroutines.withTimeout

data class SpeechPlaybackState(
    val status: TtsPlaybackStatus = TtsPlaybackStatus.IDLE,
    val sessionId: String? = null,
    val safeMessage: String? = null,
)

/** App-owned speech output. Server-side audio chunks never enter this boundary. */
interface SpeechPlayback {
    val handlesPlayback: Boolean
    val state: StateFlow<SpeechPlaybackState>
    suspend fun speak(sessionId: String, text: String)
    suspend fun stop()
    fun close()
}

/** Used by isolated tests and hosts that intentionally do not provide audible output. */
class NoOpSpeechPlayback : SpeechPlayback {
    override val handlesPlayback = false
    private val mutableState = MutableStateFlow(SpeechPlaybackState())
    override val state: StateFlow<SpeechPlaybackState> = mutableState.asStateFlow()

    override suspend fun speak(sessionId: String, text: String) {
        require(text.isNotBlank()) { "Speech text must not be blank." }
    }

    override suspend fun stop() = Unit
    override fun close() = Unit
}

/**
 * Android's installed TextToSpeech engine, kept behind a testable app boundary.
 * Input text is never logged or placed in an utterance identifier.
 */
class AndroidTextToSpeechPlayback(context: Context) : SpeechPlayback {
    override val handlesPlayback = true
    private val appContext = context.applicationContext
    private val mutex = Mutex()
    private val generation = AtomicLong()
    private val mutableState = MutableStateFlow(SpeechPlaybackState())
    override val state: StateFlow<SpeechPlaybackState> = mutableState.asStateFlow()

    @Volatile
    private var engine: TextToSpeech? = null

    @Volatile
    private var activePrefix: String? = null

    @Volatile
    private var activeFinalUtterance: String? = null

    @Volatile
    private var activeSessionId: String? = null

    override suspend fun speak(sessionId: String, text: String) {
        val normalized = text.trim()
        require(sessionId.isNotBlank()) { "A voice session is required for speech playback." }
        require(normalized.isNotEmpty()) { "Speech text must not be blank." }
        require(normalized.length <= MAX_LOGICAL_TEXT_CHARS) { "Speech text exceeds the local playback limit." }

        mutex.withLock {
            mutableState.value = SpeechPlaybackState(TtsPlaybackStatus.BUFFERING, sessionId)
            val player = ensureEngine()
            val currentGeneration = generation.incrementAndGet()
            val prefix = "veqri-$currentGeneration-"
            val chunks = splitSpeechText(normalized, TextToSpeech.getMaxSpeechInputLength())
            activePrefix = prefix
            activeSessionId = sessionId
            activeFinalUtterance = prefix + (chunks.lastIndex)

            try {
                chunks.forEachIndexed { index, chunk ->
                    val queueMode = if (index == 0) TextToSpeech.QUEUE_FLUSH else TextToSpeech.QUEUE_ADD
                    val result = player.speak(chunk, queueMode, null, prefix + index)
                    check(result == TextToSpeech.SUCCESS) { "The installed speech engine rejected playback." }
                }
            } catch (_: Exception) {
                player.stop()
                clearActivePlayback()
                mutableState.value = SpeechPlaybackState(
                    status = TtsPlaybackStatus.FAILED,
                    sessionId = sessionId,
                    safeMessage = "The installed speech engine could not start playback.",
                )
                throw SpeechPlaybackException("The installed speech engine could not start playback.")
            }
        }
    }

    override suspend fun stop() {
        mutex.withLock {
            val wasActive = mutableState.value.status in setOf(
                TtsPlaybackStatus.BUFFERING,
                TtsPlaybackStatus.SPEAKING,
            )
            generation.incrementAndGet()
            clearActivePlayback()
            engine?.stop()
            mutableState.value = SpeechPlaybackState(
                status = if (wasActive) TtsPlaybackStatus.INTERRUPTED else TtsPlaybackStatus.IDLE,
            )
        }
    }

    override fun close() {
        generation.incrementAndGet()
        clearActivePlayback()
        engine?.stop()
        engine?.shutdown()
        engine = null
        mutableState.value = SpeechPlaybackState()
    }

    private suspend fun ensureEngine(): TextToSpeech {
        engine?.let { return it }
        val initialized = CompletableDeferred<Int>()
        val created = TextToSpeech(appContext) { status -> initialized.complete(status) }
        engine = created
        val status = runCatching { withTimeout(INIT_TIMEOUT_MILLIS) { initialized.await() } }
            .getOrElse {
                created.shutdown()
                engine = null
                mutableState.value = SpeechPlaybackState(
                    status = TtsPlaybackStatus.FAILED,
                    safeMessage = "No usable Android speech engine is available.",
                )
                throw SpeechPlaybackException("No usable Android speech engine is available.")
            }
        if (status != TextToSpeech.SUCCESS) {
            created.shutdown()
            engine = null
            mutableState.value = SpeechPlaybackState(
                status = TtsPlaybackStatus.FAILED,
                safeMessage = "No usable Android speech engine is available.",
            )
            throw SpeechPlaybackException("No usable Android speech engine is available.")
        }
        val languageStatus = created.setLanguage(Locale.getDefault())
        if (languageStatus == TextToSpeech.LANG_MISSING_DATA ||
            languageStatus == TextToSpeech.LANG_NOT_SUPPORTED
        ) {
            created.shutdown()
            engine = null
            mutableState.value = SpeechPlaybackState(
                status = TtsPlaybackStatus.FAILED,
                safeMessage = "The installed speech engine does not support the device language.",
            )
            throw SpeechPlaybackException("The installed speech engine does not support the device language.")
        }
        val offlineVoice = runCatching {
            created.voices
                .filterNot { it.isNetworkConnectionRequired }
                .filterNot { TextToSpeech.Engine.KEY_FEATURE_NOT_INSTALLED in it.features }
                .filter { it.locale.language == Locale.getDefault().language }
                .maxWithOrNull(compareBy({ it.locale == Locale.getDefault() }, { it.quality }, { -it.latency }))
        }.getOrNull()
        if (offlineVoice == null || created.setVoice(offlineVoice) != TextToSpeech.SUCCESS) {
            created.shutdown()
            engine = null
            mutableState.value = SpeechPlaybackState(
                status = TtsPlaybackStatus.FAILED,
                safeMessage = "No installed offline voice is available for the device language.",
            )
            throw SpeechPlaybackException("No installed offline voice is available for the device language.")
        }
        created.setOnUtteranceProgressListener(progressListener)
        return created
    }

    private val progressListener = object : UtteranceProgressListener() {
        override fun onStart(utteranceId: String?) {
            if (!isCurrent(utteranceId)) return
            mutableState.value = SpeechPlaybackState(
                status = TtsPlaybackStatus.SPEAKING,
                sessionId = activeSessionId,
            )
        }

        override fun onDone(utteranceId: String?) {
            if (utteranceId == null || utteranceId != activeFinalUtterance) return
            clearActivePlayback()
            mutableState.value = SpeechPlaybackState()
        }

        @Deprecated("Deprecated by Android but still required by UtteranceProgressListener")
        override fun onError(utteranceId: String?) = failCurrent(utteranceId)

        override fun onError(utteranceId: String?, errorCode: Int) = failCurrent(utteranceId)

        override fun onStop(utteranceId: String?, interrupted: Boolean) {
            if (!isCurrent(utteranceId)) return
            clearActivePlayback()
            mutableState.value = SpeechPlaybackState(
                status = if (interrupted) TtsPlaybackStatus.INTERRUPTED else TtsPlaybackStatus.IDLE,
            )
        }
    }

    private fun failCurrent(utteranceId: String?) {
        if (!isCurrent(utteranceId)) return
        val sessionId = activeSessionId
        clearActivePlayback()
        mutableState.value = SpeechPlaybackState(
            status = TtsPlaybackStatus.FAILED,
            sessionId = sessionId,
            safeMessage = "The installed speech engine stopped unexpectedly.",
        )
    }

    private fun isCurrent(utteranceId: String?): Boolean =
        utteranceId != null && activePrefix?.let(utteranceId::startsWith) == true

    private fun clearActivePlayback() {
        activePrefix = null
        activeFinalUtterance = null
        activeSessionId = null
    }

    companion object {
        const val MAX_LOGICAL_TEXT_CHARS = 12 * 1024
        private const val INIT_TIMEOUT_MILLIS = 5_000L
    }
}

class SpeechPlaybackException(message: String) : IllegalStateException(message)

internal fun splitSpeechText(text: String, maxChars: Int): List<String> {
    require(maxChars >= 2) { "Speech chunks must allow at least one Unicode code point." }
    val normalized = text.trim()
    if (normalized.isEmpty()) return emptyList()
    val chunks = mutableListOf<String>()
    var start = 0
    while (start < normalized.length) {
        var end = minOf(start + maxChars, normalized.length)
        if (end < normalized.length &&
            Character.isHighSurrogate(normalized[end - 1]) &&
            Character.isLowSurrogate(normalized[end])
        ) {
            end--
        }
        if (end < normalized.length) {
            val candidate = normalized.substring(start, end)
            val naturalBreak = candidate.indexOfLast { it.isWhitespace() || it in ".!?;" }
            if (naturalBreak >= maxChars / 2) end = start + naturalBreak + 1
        }
        chunks += normalized.substring(start, end)
        start = end
    }
    return chunks
}
