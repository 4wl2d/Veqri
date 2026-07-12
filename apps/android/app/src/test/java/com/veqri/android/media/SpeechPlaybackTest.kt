package com.veqri.android.media

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class SpeechPlaybackTest {
    @Test
    fun `speech chunks preserve text and unicode boundaries`() {
        val text = "alpha beta 🚀 gamma delta epsilon"
        val chunks = splitSpeechText(text, maxChars = 12)

        assertEquals(text, chunks.joinToString(""))
        assertTrue(chunks.all { it.length <= 12 })
        assertFalse(chunks.any { it.lastOrNull()?.isHighSurrogate() == true })
        assertFalse(chunks.any { it.firstOrNull()?.isLowSurrogate() == true })
    }

    @Test
    fun `blank speech has no chunks`() {
        assertTrue(splitSpeechText("   ", maxChars = 8).isEmpty())
    }
}
