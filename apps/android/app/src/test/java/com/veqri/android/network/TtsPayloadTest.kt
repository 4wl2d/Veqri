package com.veqri.android.network

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Test

class TtsPayloadTest {
    @Test
    fun `full tts text is bounded before playback`() {
        val oversized = "x".repeat(CoreTransport.MAX_TTS_TEXT_CHARS + 100)
        assertEquals(CoreTransport.MAX_TTS_TEXT_CHARS, boundTtsText(oversized).length)
    }

    @Test
    fun `tts text bound preserves unicode code points`() {
        val oversized = "x".repeat(CoreTransport.MAX_TTS_TEXT_CHARS - 1) + "😀more"

        val bounded = boundTtsText(oversized)

        assertEquals(CoreTransport.MAX_TTS_TEXT_CHARS - 1, bounded.length)
        assertFalse(Character.isHighSurrogate(bounded.last()))
    }
}
