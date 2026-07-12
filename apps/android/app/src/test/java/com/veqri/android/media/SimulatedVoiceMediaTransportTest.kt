package com.veqri.android.media

import kotlinx.coroutines.test.runTest
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class SimulatedVoiceMediaTransportTest {
    @Test
    fun `simulator never claims a real session description or audio`() = runTest {
        val transport = SimulatedVoiceMediaTransport()

        val localDescription = transport.start(MediaSessionConfig("session-1"))
        transport.setMuted(true)
        transport.interruptPlayback()

        assertNull(localDescription)
        assertTrue((transport.state.value as MediaTransportState.Active).isSimulated)
        assertTrue(transport.isMutedForTest())
        assertTrue(transport.wasInterruptedForTest())
    }
}
