package com.veqri.android.data

import com.veqri.android.MemoryCredentialStore
import kotlinx.coroutines.test.runTest
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertFalse
import org.junit.Test

class DeviceCredentialStoreRotationTest {
    @Test
    fun `candidate slot preserves active credential until atomic promotion`() = runTest {
        val store = MemoryCredentialStore()
        val active = DeviceCredential(
            deviceId = "device",
            accessToken = "active-secret",
            coreBaseUrl = "http://10.0.2.2:8080",
            issuedAtEpochMillis = 1,
            keyVersion = 1,
        )
        val candidate = CredentialRotationCandidate(
            deviceId = active.deviceId,
            accessToken = "candidate-secret",
            coreBaseUrl = active.coreBaseUrl,
            keyVersion = 2,
            preparedAtEpochMillis = 2,
            expiresAtEpochMillis = 302_000,
            correlationId = "correlation",
        )

        store.save(active)
        store.saveRotationCandidate(candidate)

        assertEquals(1, store.read()?.keyVersion)
        assertEquals(2, store.readRotationCandidate()?.keyVersion)
        assertFalse(active.toString().contains("active-secret"))
        assertFalse(candidate.toString().contains("candidate-secret"))
        val promoted = store.promoteRotationCandidate(expectedKeyVersion = 2)
        assertEquals(2, promoted.keyVersion)
        assertEquals(2, store.read()?.keyVersion)
        assertNull(store.readRotationCandidate())
        assertEquals(
            listOf("save-active:1", "save-candidate:2", "promote:2"),
            store.operations,
        )
    }

    @Test
    fun `clearing candidate never clears active slot`() = runTest {
        val store = MemoryCredentialStore()
        val active = DeviceCredential("device", "active-secret", "http://10.0.2.2:8080", 1, 1)
        store.save(active)
        store.rotationCandidate = CredentialRotationCandidate(
            "device",
            "candidate-secret",
            active.coreBaseUrl,
            2,
            2,
            302_000,
            "correlation",
        )

        store.clearRotationCandidate()

        assertEquals(active, store.read())
        assertNull(store.readRotationCandidate())
    }
}
