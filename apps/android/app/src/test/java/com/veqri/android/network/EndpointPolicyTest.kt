package com.veqri.android.network

import org.junit.Assert.assertEquals
import org.junit.Assert.assertThrows
import org.junit.Test

class EndpointPolicyTest {
    @Test
    fun `lan and remote endpoints require tls`() {
        assertEquals(
            "https://192.168.1.10:8443",
            EndpointPolicy.requireAllowedBaseUrl("https://192.168.1.10:8443/"),
        )
        assertThrows(IllegalArgumentException::class.java) {
            EndpointPolicy.requireAllowedBaseUrl("http://192.168.1.10:8443")
        }
    }

    @Test
    fun `loopback development allows cleartext without embedded credentials`() {
        assertEquals(
            "http://10.0.2.2:8080",
            EndpointPolicy.requireAllowedBaseUrl("http://10.0.2.2:8080"),
        )
		assertThrows(IllegalArgumentException::class.java) {
			EndpointPolicy.requireAllowedBaseUrl("http://127.0.0.1:8080")
		}
        assertThrows(IllegalArgumentException::class.java) {
            EndpointPolicy.requireAllowedBaseUrl("http://token@10.0.2.2:8080")
        }
    }

    @Test
    fun `release policy rejects emulator alias and localhost cleartext`() {
        assertThrows(IllegalArgumentException::class.java) {
            EndpointPolicy.requireAllowedBaseUrl("http://10.0.2.2:8080", debugBuild = false)
        }
        assertThrows(IllegalArgumentException::class.java) {
            EndpointPolicy.requireAllowedBaseUrl("http://localhost:8080", debugBuild = false)
        }
        assertEquals(
            "https://10.0.2.2:8443",
            EndpointPolicy.requireAllowedBaseUrl("https://10.0.2.2:8443", debugBuild = false),
        )
    }
}
