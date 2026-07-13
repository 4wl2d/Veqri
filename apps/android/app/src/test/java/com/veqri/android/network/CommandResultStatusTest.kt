package com.veqri.android.network

import org.junit.Assert.assertFalse
import org.junit.Assert.assertThrows
import org.junit.Assert.assertTrue
import org.junit.Test

class CommandResultStatusTest {
    @Test
    fun `only explicit committed and rejected command results are accepted`() {
        assertTrue(commandResultCommitted("COMMITTED"))
        assertFalse(commandResultCommitted("REJECTED"))
        assertThrows(IllegalArgumentException::class.java) {
            commandResultCommitted("PENDING")
        }
    }
}
