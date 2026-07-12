package com.veqri.android.network

import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class SocketGenerationTest {
    @Test
    fun `delayed old socket snapshot cannot authorize replacement generation`() {
        assertFalse(shouldDeliverSocketEvent(eventGeneration = 7, activeGeneration = 9))
        assertTrue(shouldDeliverSocketEvent(eventGeneration = 9, activeGeneration = 9))
    }

    @Test
    fun `transport internal event survives credential socket handoff`() {
        assertTrue(shouldDeliverSocketEvent(eventGeneration = 0, activeGeneration = 12))
    }
}
