package com.veqri.android.network

import com.veqri.android.data.ConnectionStatus
import com.veqri.android.data.ApprovalStatus
import com.veqri.android.data.TaskStatus
import com.veqri.android.data.TtsPlaybackStatus
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.flow.collect
import kotlinx.coroutines.launch
import kotlinx.coroutines.test.UnconfinedTestDispatcher
import kotlinx.coroutines.test.runCurrent
import kotlinx.coroutines.test.runTest
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

@OptIn(ExperimentalCoroutinesApi::class)
class FakeCoreTransportTest {
    @Test
    fun `pairing code is one time code shaped and deterministic`() = runTest {
        val transport = transport()

        val rejected = transport.pair(pairingRequest("654321"))
        val accepted = transport.pair(pairingRequest(FakeCoreTransport.DEVELOPMENT_PAIRING_CODE))

        assertTrue(rejected.isFailure)
        assertTrue(accepted.isSuccess)
        assertTrue(accepted.getOrThrow().deviceId.startsWith("android-id-"))
    }

    @Test
    fun `text request streams task progress and final answer`() = runTest {
        val transport = transport()
        val received = mutableListOf<CoreEvent>()
        backgroundScope.launch(UnconfinedTestDispatcher(testScheduler)) {
            transport.events.collect(received::add)
        }
        connect(transport)

        transport.send(CoreCommand.SendText("command-1", null, "Inspect the repository"))
        runCurrent()

        val taskEvents = received.filterIsInstance<CoreEvent.TaskChanged>()
        assertEquals(
            listOf(TaskStatus.QUEUED, TaskStatus.RUNNING, TaskStatus.COMPLETED),
            taskEvents.map { it.task.status },
        )
        assertEquals(2, received.filterIsInstance<CoreEvent.MessageAdded>().size)
        assertTrue(received.filterIsInstance<CoreEvent.MessageAdded>().last().message.text.contains("Simulated result"))
    }

    @Test
    fun `barge in interrupts tts but never cancels delegated task`() = runTest {
        val transport = transport()
        val received = mutableListOf<CoreEvent>()
        backgroundScope.launch(UnconfinedTestDispatcher(testScheduler)) {
            transport.events.collect(received::add)
        }
        connect(transport)
        transport.send(CoreCommand.StartCall("call-command"))
        val sessionId = received.filterIsInstance<CoreEvent.VoiceChanged>().last().session.id
        transport.send(CoreCommand.SendText("task-command", null, "Run a long task"))

        transport.send(CoreCommand.InterruptTts("interrupt-command", sessionId))
        runCurrent()

        assertEquals(
            TtsPlaybackStatus.INTERRUPTED,
            received.filterIsInstance<CoreEvent.TtsChanged>().last().status,
        )
        assertFalse(received.filterIsInstance<CoreEvent.TaskChanged>().any {
            it.task.status == TaskStatus.CANCELLED || it.task.status == TaskStatus.CANCEL_REQUESTED
        })
        assertEquals(TaskStatus.COMPLETED, received.filterIsInstance<CoreEvent.TaskChanged>().last().task.status)
    }

    @Test
    fun `brief network loss reconnects without replacing conversation`() = runTest {
        val transport = transport()
        val received = mutableListOf<CoreEvent>()
        backgroundScope.launch(UnconfinedTestDispatcher(testScheduler)) {
            transport.events.collect(received::add)
        }
        connect(transport)
        transport.send(CoreCommand.SendText("first", null, "First"))
        val firstConversation = received.filterIsInstance<CoreEvent.MessageAdded>().last().message.conversationId

        transport.simulateNetworkLoss()
        transport.send(CoreCommand.SendText("second", null, "Second"))
        runCurrent()

        val secondConversation = received.filterIsInstance<CoreEvent.MessageAdded>().last().message.conversationId
        assertEquals(firstConversation, secondConversation)
        assertTrue(transport.connection.value is ConnectionStatus.Connected)
    }

    @Test
    fun `approval denial keeps state changing operation from completing`() = runTest {
        val transport = transport()
        val received = mutableListOf<CoreEvent>()
        backgroundScope.launch(UnconfinedTestDispatcher(testScheduler)) {
            transport.events.collect(received::add)
        }
        connect(transport)
        transport.send(CoreCommand.SendText("risky", null, "Delete this after approval"))
        val pending = received.filterIsInstance<CoreEvent.ApprovalChanged>().single().approval

        transport.send(CoreCommand.ResolveApproval("deny", pending.id, approved = false))
        runCurrent()

        assertEquals(
            ApprovalStatus.DENIED,
            received.filterIsInstance<CoreEvent.ApprovalChanged>().last().approval.status,
        )
        assertEquals(TaskStatus.CANCELLED, received.filterIsInstance<CoreEvent.TaskChanged>().last().task.status)
        assertFalse(received.filterIsInstance<CoreEvent.TaskChanged>().any {
            it.task.status == TaskStatus.COMPLETED
        })
    }

    @Test
    fun `fake transport models two phase credential promotion and idempotent confirm`() = runTest {
        val transport = transport()
        val active = transport.pair(
            pairingRequest(FakeCoreTransport.DEVELOPMENT_PAIRING_CODE),
        ).getOrThrow()
        transport.connect(active)

        val candidate = transport.prepareCredentialRotation(active)
        transport.armCredentialRotation(candidate)
        val first = transport.confirmCredentialRotation(candidate)
        val repeated = transport.confirmCredentialRotation(candidate)
        val oldCredentialFailure = runCatching { transport.cancelCredentialRotation(active) }.exceptionOrNull()

        assertEquals(2, candidate.keyVersion)
        assertFalse(first.alreadyConfirmed)
        assertTrue(repeated.alreadyConfirmed)
        assertEquals(
            CredentialRotationFailureKind.UNAUTHORIZED,
            (oldCredentialFailure as CredentialRotationException).kind,
        )
    }

    private fun transport(): FakeCoreTransport {
        var nextId = 0
        return FakeCoreTransport(
            idSource = IdSource { "id-${nextId++}" },
            clock = EpochClock { 1_700_000_000_000L + nextId },
        )
    }

    private suspend fun connect(transport: FakeCoreTransport) {
        val credential = transport.pair(pairingRequest(FakeCoreTransport.DEVELOPMENT_PAIRING_CODE)).getOrThrow()
        transport.connect(credential)
    }

    private fun pairingRequest(code: String) = PairingRequest(
        coreBaseUrl = "http://10.0.2.2:8080",
        oneTimeCode = code,
        deviceName = "Test Android",
    )
}
