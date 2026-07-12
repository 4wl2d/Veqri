package com.veqri.android

import androidx.test.core.app.ApplicationProvider
import androidx.test.ext.junit.runners.AndroidJUnit4
import com.veqri.android.data.AndroidKeystoreCredentialStore
import com.veqri.android.data.CredentialRotationCandidate
import com.veqri.android.data.DeviceCredential
import kotlinx.coroutines.runBlocking
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Before
import org.junit.Test
import org.junit.runner.RunWith

@RunWith(AndroidJUnit4::class)
class AndroidKeystoreCredentialStoreTest {
    private val store by lazy {
        AndroidKeystoreCredentialStore(ApplicationProvider.getApplicationContext())
    }

    @Before
    fun clearBefore() = runBlocking { store.clear() }

    @After
    fun clearAfter() = runBlocking { store.clear() }

    @Test
    fun activeAndCandidateRemainSeparateUntilPromotion() = runBlocking {
        val active = DeviceCredential(
            deviceId = "instrumented-device",
            accessToken = "active-instrumented-secret",
            coreBaseUrl = "https://core.example.test",
            issuedAtEpochMillis = 1_000,
            keyVersion = 1,
        )
        val candidate = CredentialRotationCandidate(
            deviceId = active.deviceId,
            accessToken = "candidate-instrumented-secret",
            coreBaseUrl = active.coreBaseUrl,
            keyVersion = 2,
            preparedAtEpochMillis = 2_000,
            expiresAtEpochMillis = 302_000,
            correlationId = "instrumented-correlation",
        )

        store.save(active)
        store.saveRotationCandidate(candidate)
        assertEquals(active, store.read())
        assertEquals(candidate, store.readRotationCandidate())

        val promoted = store.promoteRotationCandidate(2)
        assertEquals(candidate.toPromotedCredential(), promoted)
        assertEquals(promoted, store.read())
        assertNull(store.readRotationCandidate())
    }
}
