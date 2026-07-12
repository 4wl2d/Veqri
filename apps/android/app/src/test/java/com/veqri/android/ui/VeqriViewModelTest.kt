package com.veqri.android.ui

import com.veqri.android.FakeAudioRoutes
import com.veqri.android.MainDispatcherRule
import com.veqri.android.MemoryConversationCache
import com.veqri.android.MemoryCredentialStore
import com.veqri.android.MemoryPreferenceStore
import com.veqri.android.ManualCoreTransport
import com.veqri.android.RecordingCallController
import com.veqri.android.data.TaskRecord
import com.veqri.android.data.TaskStatus
import com.veqri.android.data.DeviceCredential
import com.veqri.android.data.VeqriRepository
import com.veqri.android.media.SimulatedVoiceMediaTransport
import com.veqri.android.network.EpochClock
import com.veqri.android.network.CoreEvent
import com.veqri.android.network.FakeCoreTransport
import com.veqri.android.network.IdSource
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.flow.collect
import kotlinx.coroutines.launch
import kotlinx.coroutines.test.UnconfinedTestDispatcher
import kotlinx.coroutines.test.runCurrent
import kotlinx.coroutines.test.runTest
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Rule
import org.junit.Test

@OptIn(ExperimentalCoroutinesApi::class)
class VeqriViewModelTest {
    @get:Rule
    val mainDispatcherRule = MainDispatcherRule()

    @Test
    fun `view model maps transport progress into render models and navigation`() = runTest(
        context = mainDispatcherRule.dispatcher,
    ) {
        var nextId = 0
        val routes = FakeAudioRoutes()
        val repository = VeqriRepository(
            scope = backgroundScope,
            transport = FakeCoreTransport(
                idSource = IdSource { "id-${nextId++}" },
                clock = EpochClock { 1_700_000_000_000L + nextId },
            ),
            credentialStore = MemoryCredentialStore(),
            preferenceStore = MemoryPreferenceStore(),
            cache = MemoryConversationCache(),
            mediaTransport = SimulatedVoiceMediaTransport(routes),
            audioRoutes = routes,
            calls = RecordingCallController(),
            idSource = IdSource { "command-${nextId++}" },
        )
        val viewModel = VeqriViewModel(repository)
        backgroundScope.launch(UnconfinedTestDispatcher(testScheduler)) { viewModel.uiState.collect() }
        runCurrent()

        viewModel.dispatch(VeqriAction.Pair("http://10.0.2.2:8080", "123456", "Test phone"))
        viewModel.dispatch(VeqriAction.SendText("Delegate this"))
        viewModel.dispatch(VeqriAction.Navigate(AppDestination.TASKS))
        runCurrent()

        assertTrue(viewModel.uiState.value.isPaired)
        assertEquals(AppDestination.TASKS, viewModel.uiState.value.destination)
        assertEquals(1, viewModel.uiState.value.tasks.size)
        assertEquals(100, viewModel.uiState.value.tasks.single().progressPercent)
        assertEquals(2, viewModel.uiState.value.messages.size)
    }

    @Test
    fun `retry visibility follows Core eligibility instead of terminal status alone`() = runTest(
        context = mainDispatcherRule.dispatcher,
    ) {
        val transport = ManualCoreTransport()
        val routes = FakeAudioRoutes()
        val repository = VeqriRepository(
            scope = backgroundScope,
            transport = transport,
            credentialStore = MemoryCredentialStore(),
            preferenceStore = MemoryPreferenceStore(),
            cache = MemoryConversationCache(),
            mediaTransport = SimulatedVoiceMediaTransport(routes),
            audioRoutes = routes,
            calls = RecordingCallController(),
        )
        val viewModel = VeqriViewModel(repository)
        backgroundScope.launch(UnconfinedTestDispatcher(testScheduler)) { viewModel.uiState.collect() }
        runCurrent()
        transport.emit(
            CoreEvent.AuthoritativeSnapshot(
                eventId = "snapshot-event",
                correlationId = "snapshot",
                snapshotId = "snapshot-1",
                conversationId = null,
                transcriptRetention = true,
                messages = emptyList(),
                tasks = listOf(
                    failedTask("shell", canRetry = false),
                    failedTask("eligible", canRetry = true),
                ),
                approvals = emptyList(),
                voiceSession = null,
            ),
        )
        runCurrent()

        assertFalse(viewModel.uiState.value.tasks.single { it.id == "shell" }.canRetry)
        assertTrue(viewModel.uiState.value.tasks.single { it.id == "eligible" }.canRetry)
    }

    @Test
    fun `tasks are ordered by authoritative priority then recency`() = runTest(
        context = mainDispatcherRule.dispatcher,
    ) {
        val transport = ManualCoreTransport()
        val routes = FakeAudioRoutes()
        val repository = VeqriRepository(
            scope = backgroundScope,
            transport = transport,
            credentialStore = MemoryCredentialStore(),
            preferenceStore = MemoryPreferenceStore(),
            cache = MemoryConversationCache(),
            mediaTransport = SimulatedVoiceMediaTransport(routes),
            audioRoutes = routes,
            calls = RecordingCallController(),
        )
        val viewModel = VeqriViewModel(repository)
        backgroundScope.launch(UnconfinedTestDispatcher(testScheduler)) { viewModel.uiState.collect() }
        runCurrent()
        transport.emit(
            CoreEvent.AuthoritativeSnapshot(
                eventId = "snapshot-event",
                correlationId = "snapshot",
                snapshotId = "snapshot-1",
                conversationId = null,
                transcriptRetention = true,
                messages = emptyList(),
                tasks = listOf(
                    failedTask("old-high", canRetry = true, priority = 50, updatedAtEpochMillis = 1),
                    failedTask("new-low", canRetry = true, priority = 0, updatedAtEpochMillis = 100),
                    failedTask("new-high", canRetry = true, priority = 50, updatedAtEpochMillis = 2),
                ),
                approvals = emptyList(),
                voiceSession = null,
            ),
        )
        runCurrent()

        assertEquals(
            listOf("new-high", "old-high", "new-low"),
            viewModel.uiState.value.tasks.map { it.id },
        )
    }

    @Test
    fun `manual rotate action publishes safe credential render state`() = runTest(
        context = mainDispatcherRule.dispatcher,
    ) {
        val credentials = MemoryCredentialStore().apply {
            value = DeviceCredential(
                deviceId = "device",
                accessToken = "active-secret",
                coreBaseUrl = "http://10.0.2.2:8080",
                issuedAtEpochMillis = 1,
                keyVersion = 1,
            )
        }
        val transport = ManualCoreTransport()
        val routes = FakeAudioRoutes()
        val repository = VeqriRepository(
            scope = backgroundScope,
            transport = transport,
            credentialStore = credentials,
            preferenceStore = MemoryPreferenceStore(),
            cache = MemoryConversationCache(),
            mediaTransport = SimulatedVoiceMediaTransport(routes),
            audioRoutes = routes,
            calls = RecordingCallController(),
        )
        val viewModel = VeqriViewModel(repository)
        backgroundScope.launch(UnconfinedTestDispatcher(testScheduler)) { viewModel.uiState.collect() }
        runCurrent()

        viewModel.dispatch(VeqriAction.RotateCredential)
        runCurrent()

        assertEquals("Rotation complete", viewModel.uiState.value.credentialRotation.label)
        assertEquals("Active key version 2", viewModel.uiState.value.credentialRotation.keyVersionLabel)
        assertTrue(viewModel.uiState.value.credentialRotation.canRotate)
        assertFalse(viewModel.uiState.value.credentialRotation.detail.contains("secret"))
    }

    private fun failedTask(
        id: String,
        canRetry: Boolean,
        priority: Int = 0,
        updatedAtEpochMillis: Long = 2,
    ) = TaskRecord(
        id = id,
        rootTaskId = id,
        conversationId = "conversation",
        goal = id,
        assignedAgent = "agent",
        status = TaskStatus.FAILED,
        progressPercent = 50,
        summary = id,
        createdAtEpochMillis = 1,
        updatedAtEpochMillis = updatedAtEpochMillis,
        correlationId = "correlation-$id",
        canRetry = canRetry,
        priority = priority,
    )
}
