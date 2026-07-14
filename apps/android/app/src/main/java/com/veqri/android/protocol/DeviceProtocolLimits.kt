package com.veqri.android.protocol

import java.nio.charset.StandardCharsets

internal const val MAX_TTS_TEXT_UTF8_BYTES = 12 * 1024

internal fun utf8Size(value: String): Int = value.toByteArray(StandardCharsets.UTF_8).size

/** Returns the longest whole-code-point prefix that fits the Android TTS wire limit. */
internal fun boundTtsTextUtf8(value: String): String {
    var index = 0
    var byteCount = 0
    while (index < value.length) {
        val current = value[index]
        val codePoint = when {
            Character.isHighSurrogate(current) -> {
                require(index + 1 < value.length && Character.isLowSurrogate(value[index + 1])) {
                    "TTS text contains an unpaired high surrogate."
                }
                Character.toCodePoint(current, value[index + 1])
            }
            Character.isLowSurrogate(current) -> {
                throw IllegalArgumentException("TTS text contains an unpaired low surrogate.")
            }
            else -> current.code
        }
        val encodedBytes = when {
            codePoint <= 0x7f -> 1
            codePoint <= 0x7ff -> 2
            codePoint <= 0xffff -> 3
            else -> 4
        }
        if (byteCount + encodedBytes > MAX_TTS_TEXT_UTF8_BYTES) break
        byteCount += encodedBytes
        index += Character.charCount(codePoint)
    }
    return if (index == value.length) value else value.substring(0, index)
}

internal fun requireTtsTextWithinLimit(value: String) {
    require(boundTtsTextUtf8(value) == value && utf8Size(value) <= MAX_TTS_TEXT_UTF8_BYTES) {
        "Speech text exceeds the local playback limit."
    }
}
