package com.veqri.android.network

import com.veqri.android.protocol.MAX_TTS_TEXT_UTF8_BYTES
import com.veqri.android.protocol.boundTtsTextUtf8
import com.veqri.android.protocol.requireTtsTextWithinLimit
import com.veqri.android.protocol.utf8Size
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertSame
import org.junit.Assert.assertThrows
import org.junit.Test

class TtsPayloadTest {
    @Test
    fun `exact utf8 boundaries are preserved`() {
        val exactValues = listOf(
            "x".repeat(MAX_TTS_TEXT_UTF8_BYTES),
            "Ж".repeat(MAX_TTS_TEXT_UTF8_BYTES / 2),
            "界".repeat(MAX_TTS_TEXT_UTF8_BYTES / 3),
            "😀".repeat(MAX_TTS_TEXT_UTF8_BYTES / 4),
        )

        exactValues.forEach { value ->
            assertEquals(MAX_TTS_TEXT_UTF8_BYTES, utf8Size(value))
            assertSame(value, boundTtsTextUtf8(value))
            requireTtsTextWithinLimit(value)
        }
    }

    @Test
    fun `oversized utf8 text is reduced to the maximal whole code point prefix`() {
        val cases = listOf(
            "x".repeat(MAX_TTS_TEXT_UTF8_BYTES) + "x" to "x".repeat(MAX_TTS_TEXT_UTF8_BYTES),
            "Ж".repeat(MAX_TTS_TEXT_UTF8_BYTES / 2 + 1) to "Ж".repeat(MAX_TTS_TEXT_UTF8_BYTES / 2),
            "界".repeat(MAX_TTS_TEXT_UTF8_BYTES / 3 + 1) to "界".repeat(MAX_TTS_TEXT_UTF8_BYTES / 3),
            "😀".repeat(MAX_TTS_TEXT_UTF8_BYTES / 4 + 1) to "😀".repeat(MAX_TTS_TEXT_UTF8_BYTES / 4),
            "x".repeat(MAX_TTS_TEXT_UTF8_BYTES - 1) + "😀" to
                "x".repeat(MAX_TTS_TEXT_UTF8_BYTES - 1),
        )

        cases.forEach { (value, expected) ->
            val bounded = boundTtsTextUtf8(value)
            assertEquals(expected, bounded)
            assertEquals(true, utf8Size(bounded) <= MAX_TTS_TEXT_UTF8_BYTES)
            assertFalse(bounded.lastOrNull()?.isHighSurrogate() == true)
            assertFalse(bounded.firstOrNull()?.isLowSurrogate() == true)
            assertThrows(IllegalArgumentException::class.java) {
                requireTtsTextWithinLimit(value)
            }
        }
    }

    @Test
    fun `malformed utf16 is rejected before protobuf or playback`() {
        assertThrows(IllegalArgumentException::class.java) {
            boundTtsTextUtf8("valid\uD83D")
        }
        assertThrows(IllegalArgumentException::class.java) {
            boundTtsTextUtf8("\uDE00invalid")
        }
    }
}
