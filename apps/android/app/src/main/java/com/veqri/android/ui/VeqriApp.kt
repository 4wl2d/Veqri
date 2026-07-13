package com.veqri.android.ui

import androidx.compose.foundation.background
import androidx.compose.foundation.gestures.detectTapGestures
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.text.KeyboardActions
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.FilterChip
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.NavigationBar
import androidx.compose.material3.NavigationBarItem
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedCard
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Surface
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.input.pointer.pointerInput
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.onClick
import androidx.compose.ui.semantics.role
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.ImeAction
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.text.input.TextFieldValue
import androidx.compose.ui.unit.dp
import com.veqri.android.data.ApprovalStatus
import com.veqri.android.data.AudioRouteKind
import com.veqri.android.data.DialogPhase
import com.veqri.android.data.MessageAuthor
import com.veqri.android.data.TtsPlaybackStatus

@Composable
fun VeqriApp(
    state: VeqriUiState,
    onAction: (VeqriAction) -> Unit,
) {
    if (!state.isPaired) {
        PairingScreen(state, onAction)
        return
    }
    MainShell(state, onAction)
    state.globalError?.let { message ->
        AlertDialog(
            onDismissRequest = { onAction(VeqriAction.ClearError) },
            confirmButton = {
                TextButton(onClick = { onAction(VeqriAction.ClearError) }) { Text("OK") }
            },
            title = { Text("Action failed") },
            text = { Text(message) },
        )
    }
}

@Composable
fun PairingScreen(
    state: VeqriUiState,
    onAction: (VeqriAction) -> Unit,
) {
    var endpointValue by rememberSaveable(stateSaver = TextFieldValue.Saver) {
        mutableStateOf(TextFieldValue(state.defaultCoreBaseUrl))
    }
    var codeValue by rememberSaveable(stateSaver = TextFieldValue.Saver) {
        mutableStateOf(TextFieldValue(""))
    }
    var deviceNameValue by rememberSaveable(stateSaver = TextFieldValue.Saver) {
        mutableStateOf(TextFieldValue("My Android"))
    }
    val pairingCodeLength = if (state.isLocalSimulator) 6 else 8
    val canPair = codeValue.text.length == pairingCodeLength && endpointValue.text.isNotBlank() &&
        deviceNameValue.text.isNotBlank() && !state.pairingInProgress
    val submitPair = {
        if (canPair) {
            onAction(VeqriAction.Pair(endpointValue.text, codeValue.text, deviceNameValue.text))
        }
    }
    Surface(modifier = Modifier.fillMaxSize()) {
        Column(
            modifier = Modifier
                .fillMaxSize()
                .verticalScroll(rememberScrollState())
                .padding(horizontal = 24.dp, vertical = 48.dp),
            verticalArrangement = Arrangement.Center,
        ) {
            Text(
                text = "Veqri",
                style = MaterialTheme.typography.displaySmall,
                fontWeight = FontWeight.Bold,
                modifier = Modifier.testTag("pairing-title"),
            )
            Text(
                text = "Pair this phone with your local Veqri Core using its short-lived one-time code.",
                style = MaterialTheme.typography.bodyLarge,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
            Spacer(Modifier.height(24.dp))
            OutlinedTextField(
                value = endpointValue,
                onValueChange = { endpointValue = it },
                label = { Text("Veqri Core URL") },
                singleLine = true,
                keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Uri, imeAction = ImeAction.Next),
                modifier = Modifier.fillMaxWidth().testTag("core-url"),
            )
            Spacer(Modifier.height(12.dp))
            OutlinedTextField(
                value = deviceNameValue,
                onValueChange = { deviceNameValue = it },
                label = { Text("Device name") },
                singleLine = true,
                keyboardOptions = KeyboardOptions(imeAction = ImeAction.Next),
                modifier = Modifier.fillMaxWidth(),
            )
            Spacer(Modifier.height(12.dp))
            OutlinedTextField(
                value = codeValue,
                onValueChange = { next ->
					codeValue = next.copy(text = next.text.filter(Char::isDigit).take(pairingCodeLength))
                },
                label = { Text("One-time code") },
                singleLine = true,
                visualTransformation = PasswordVisualTransformation(),
                keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.NumberPassword, imeAction = ImeAction.Done),
                keyboardActions = KeyboardActions(onDone = { submitPair() }),
                modifier = Modifier.fillMaxWidth().testTag("pairing-code"),
                isError = state.pairingError != null,
                supportingText = state.pairingError?.let { error -> ({ Text(error) }) },
            )
            Spacer(Modifier.height(16.dp))
            Button(
                onClick = submitPair,
                enabled = canPair,
                modifier = Modifier.fillMaxWidth().testTag("pair-button"),
            ) {
                Text(if (state.pairingInProgress) "Pairing…" else "Pair device")
            }
            if (state.isLocalSimulator) {
                Spacer(Modifier.height(16.dp))
                SimulatorNotice("Debug simulator code: 123456. It never contacts a real Core.")
            }
        }
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun MainShell(state: VeqriUiState, onAction: (VeqriAction) -> Unit) {
    Scaffold(
        topBar = {
            TopAppBar(
                title = { Text("Veqri") },
                actions = {
                    ConnectionPill(state.connection)
                    Spacer(Modifier.width(12.dp))
                },
            )
        },
        bottomBar = {
            NavigationBar {
                AppDestination.entries.forEach { destination ->
                    val badge = when (destination) {
                        AppDestination.TASKS -> state.activeTaskCount
                        AppDestination.APPROVALS -> state.pendingApprovalCount
                        else -> 0
                    }
                    NavigationBarItem(
                        selected = state.destination == destination,
                        onClick = { onAction(VeqriAction.Navigate(destination)) },
                        icon = { Text(if (badge > 0) badge.toString() else destination.label.take(1)) },
                        label = { Text(destination.label) },
                    )
                }
            }
        },
    ) { padding ->
        when (state.destination) {
            AppDestination.CONVERSATION -> ConversationScreen(state, padding, onAction)
            AppDestination.TASKS -> TasksScreen(state, padding, onAction)
            AppDestination.APPROVALS -> ApprovalsScreen(state, padding, onAction)
            AppDestination.CALL -> CallScreen(state, padding, onAction)
        }
    }
}

@Composable
private fun ConnectionPill(connection: ConnectionUi) {
    val color = when (connection.tone) {
        ConnectionTone.NEUTRAL -> MaterialTheme.colorScheme.outline
        ConnectionTone.GOOD -> Color(0xFF16855B)
        ConnectionTone.WARNING -> Color(0xFFB25E00)
        ConnectionTone.BAD -> MaterialTheme.colorScheme.error
    }
    Row(verticalAlignment = Alignment.CenterVertically) {
        Box(Modifier.size(8.dp).background(color, CircleShape))
        Spacer(Modifier.width(6.dp))
        Text(connection.label, style = MaterialTheme.typography.labelLarge)
    }
}

@Composable
private fun ConversationScreen(
    state: VeqriUiState,
    padding: PaddingValues,
    onAction: (VeqriAction) -> Unit,
) {
    var inputValue by rememberSaveable(stateSaver = TextFieldValue.Saver) {
        mutableStateOf(TextFieldValue(""))
    }
    var showForgetDialogValue by rememberSaveable { mutableStateOf(false) }
    Column(Modifier.fillMaxSize().padding(padding).padding(horizontal = 16.dp)) {
        if (state.isLocalSimulator) {
            SimulatorNotice("Local deterministic transport is active. Results and voice state are simulated.")
            Spacer(Modifier.height(8.dp))
        }
        Row(
            modifier = Modifier.fillMaxWidth(),
            verticalAlignment = Alignment.CenterVertically,
            horizontalArrangement = Arrangement.SpaceBetween,
        ) {
            Column(Modifier.weight(1f)) {
                Text("Retain voice transcript", fontWeight = FontWeight.SemiBold)
                Text(
					when {
						state.retentionChangeInProgress -> "Waiting for Core to commit the privacy change."
						!state.retentionStateAuthoritative -> "Waiting for reconnect to confirm Core's privacy state."
						else -> "Turn off to delete the current Core transcript and its cached messages on this device."
					},
                    style = MaterialTheme.typography.bodySmall,
                )
            }
            Switch(
                checked = state.retainTranscript,
                onCheckedChange = { onAction(VeqriAction.SetTranscriptRetention(it)) },
				enabled = state.retentionStateAuthoritative && !state.retentionChangeInProgress,
            )
        }
        Spacer(Modifier.height(8.dp))
        OutlinedCard(Modifier.fillMaxWidth()) {
            Column(Modifier.padding(12.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
                Text("Device credential", fontWeight = FontWeight.SemiBold)
                Text(state.credentialRotation.keyVersionLabel, style = MaterialTheme.typography.labelMedium)
                Text(state.credentialRotation.label, style = MaterialTheme.typography.bodyMedium)
                Text(
                    state.credentialRotation.detail,
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
                OutlinedButton(
                    onClick = { onAction(VeqriAction.RotateCredential) },
                    enabled = state.credentialRotation.canRotate && !state.credentialRotation.inProgress,
                    modifier = Modifier.fillMaxWidth().testTag("rotate-credential"),
                ) {
                    Text(
                        if (state.credentialRotation.inProgress) {
                            "Rotating…"
                        } else {
                            state.credentialRotation.buttonLabel
                        },
                    )
                }
            }
        }
        Spacer(Modifier.height(8.dp))
        TextButton(
            onClick = { showForgetDialogValue = true },
            modifier = Modifier.align(Alignment.End),
        ) {
            Text("Forget this device")
        }
        if (state.partialTranscript.isNotBlank() || state.finalTranscript.isNotBlank()) {
            TranscriptCard(state.partialTranscript, state.finalTranscript)
            Spacer(Modifier.height(8.dp))
        }
        LazyColumn(
            modifier = Modifier.weight(1f).fillMaxWidth(),
            verticalArrangement = Arrangement.spacedBy(8.dp),
            contentPadding = PaddingValues(vertical = 8.dp),
        ) {
            if (state.messages.isEmpty()) {
                item {
                    EmptyState(
                        title = "Start a conversation",
                        detail = "Send a request. Long-running work will continue as a task while chat stays responsive.",
                    )
                }
            }
            items(state.messages, key = MessageUi::id) { message -> MessageBubble(message) }
        }
        OutlinedTextField(
            value = inputValue,
            onValueChange = { inputValue = it },
            label = { Text("Message Veqri") },
            minLines = 1,
            maxLines = 4,
            keyboardOptions = KeyboardOptions(imeAction = ImeAction.Send),
            keyboardActions = KeyboardActions(onSend = {
                if (inputValue.text.isNotBlank()) {
                    onAction(VeqriAction.SendText(inputValue.text))
                    inputValue = TextFieldValue("")
                }
            }),
            modifier = Modifier.fillMaxWidth().testTag("message-input"),
        )
        Row(
            modifier = Modifier.fillMaxWidth().padding(vertical = 12.dp),
            horizontalArrangement = Arrangement.spacedBy(8.dp),
        ) {
            OutlinedButton(
                onClick = { onAction(VeqriAction.StartCall) },
                modifier = Modifier.weight(1f),
            ) { Text("Start call") }
            Button(
                onClick = {
                    onAction(VeqriAction.SendText(inputValue.text))
                    inputValue = TextFieldValue("")
                },
                enabled = inputValue.text.isNotBlank(),
                modifier = Modifier.weight(1f).testTag("send-message"),
            ) { Text("Send") }
        }
    }
    if (showForgetDialogValue) {
        AlertDialog(
            onDismissRequest = { showForgetDialogValue = false },
            title = { Text("Forget this device?") },
            text = {
                Text(
                    "This clears the local credential and cache. Revoke the device in Veqri Core as well if it may be compromised.",
                )
            },
            dismissButton = {
                TextButton(onClick = { showForgetDialogValue = false }) { Text("Keep") }
            },
            confirmButton = {
                Button(onClick = {
                    showForgetDialogValue = false
                    onAction(VeqriAction.ForgetLocalDevice)
                }) { Text("Forget locally") }
            },
        )
    }
}

@Composable
private fun MessageBubble(message: MessageUi) {
    val isUser = message.author == MessageAuthor.USER
    Row(
        modifier = Modifier.fillMaxWidth(),
        horizontalArrangement = if (isUser) Arrangement.End else Arrangement.Start,
    ) {
        Card(
            modifier = Modifier.fillMaxWidth(0.86f),
            colors = CardDefaults.cardColors(
                containerColor = if (isUser) {
                    MaterialTheme.colorScheme.primaryContainer
                } else {
                    MaterialTheme.colorScheme.surfaceVariant
                },
            ),
        ) {
            Column(Modifier.padding(12.dp)) {
                Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.SpaceBetween) {
                    Text(message.authorLabel, fontWeight = FontWeight.SemiBold)
                    Text(message.timestampLabel, style = MaterialTheme.typography.labelSmall)
                }
                Spacer(Modifier.height(4.dp))
                Text(message.text)
            }
        }
    }
}

@Composable
private fun TranscriptCard(partial: String, final: String) {
    OutlinedCard(Modifier.fillMaxWidth()) {
        Column(Modifier.padding(12.dp)) {
            Text("Live transcript", fontWeight = FontWeight.SemiBold)
            if (final.isNotBlank()) Text(final)
            if (partial.isNotBlank()) {
                Text(partial, color = MaterialTheme.colorScheme.onSurfaceVariant)
            }
        }
    }
}

@Composable
private fun TasksScreen(
    state: VeqriUiState,
    padding: PaddingValues,
    onAction: (VeqriAction) -> Unit,
) {
    val selected = state.selectedTask
    if (selected != null) {
        TaskDetail(selected, padding, onAction)
        return
    }
    LazyColumn(
        modifier = Modifier.fillMaxSize().padding(padding).padding(horizontal = 16.dp),
        verticalArrangement = Arrangement.spacedBy(10.dp),
        contentPadding = PaddingValues(vertical = 12.dp),
    ) {
        item { ScreenHeading("Tasks", "Inspect, cancel, or retry delegated work.") }
        if (state.tasks.isEmpty()) {
            item { EmptyState("No tasks yet", "Delegated requests will appear here with live progress.") }
        }
        items(state.tasks, key = TaskUi::id) { task ->
            TaskCard(task, onClick = { onAction(VeqriAction.SelectTask(task.id)) })
        }
    }
}

@Composable
private fun TaskDetail(task: TaskUi, padding: PaddingValues, onAction: (VeqriAction) -> Unit) {
    Column(
        modifier = Modifier.fillMaxSize().padding(padding).verticalScroll(rememberScrollState()).padding(16.dp),
        verticalArrangement = Arrangement.spacedBy(12.dp),
    ) {
        TextButton(onClick = { onAction(VeqriAction.SelectTask(null)) }) { Text("Back to tasks") }
        ScreenHeading(task.title, task.statusLabel)
        LinearProgressIndicator(
            progress = { task.progressPercent / 100f },
            modifier = Modifier.fillMaxWidth(),
        )
        Text("Agent: ${task.agentLabel}")
        Text(task.summary.ifBlank { "No summary has been reported yet." })
        Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            if (task.canCancel) {
                OutlinedButton(onClick = { onAction(VeqriAction.CancelTask(task.id)) }) {
                    Text("Cancel")
                }
            }
            if (task.canRetry) {
                Button(onClick = { onAction(VeqriAction.RetryTask(task.id)) }) { Text("Retry") }
            }
        }
    }
}

@Composable
private fun TaskCard(task: TaskUi, onClick: () -> Unit) {
    OutlinedCard(onClick = onClick, modifier = Modifier.fillMaxWidth()) {
        Column(Modifier.padding(14.dp), verticalArrangement = Arrangement.spacedBy(6.dp)) {
            Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.SpaceBetween) {
                Text(task.title, fontWeight = FontWeight.SemiBold, modifier = Modifier.weight(1f))
                Spacer(Modifier.width(8.dp))
                Text(task.statusLabel, style = MaterialTheme.typography.labelMedium)
            }
            LinearProgressIndicator(
                progress = { task.progressPercent / 100f },
                modifier = Modifier.fillMaxWidth(),
            )
            Text(task.summary, style = MaterialTheme.typography.bodySmall)
            Text("Agent: ${task.agentLabel}", style = MaterialTheme.typography.labelSmall)
        }
    }
}

@Composable
private fun ApprovalsScreen(
    state: VeqriUiState,
    padding: PaddingValues,
    onAction: (VeqriAction) -> Unit,
) {
    LazyColumn(
        modifier = Modifier.fillMaxSize().padding(padding).padding(horizontal = 16.dp),
        verticalArrangement = Arrangement.spacedBy(10.dp),
        contentPadding = PaddingValues(vertical = 12.dp),
    ) {
        item { ScreenHeading("Approvals", "Single-use, expiring decisions for state-changing tools.") }
        if (state.approvals.isEmpty()) {
            item { EmptyState("No approval requests", "Risky tool requests will wait here for you.") }
        }
        items(state.approvals, key = ApprovalUi::id) { approval ->
            ApprovalCard(approval, onAction)
        }
    }
}

@Composable
private fun ApprovalCard(approval: ApprovalUi, onAction: (VeqriAction) -> Unit) {
    OutlinedCard(Modifier.fillMaxWidth()) {
        Column(Modifier.padding(14.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
            Text(approval.title, fontWeight = FontWeight.SemiBold)
            Text("Risk: ${approval.riskLabel}", color = MaterialTheme.colorScheme.error)
            Text(
                "Permission: ${approval.requestedScopes.joinToString().ifBlank { "unspecified (deny recommended)" }}",
                fontWeight = FontWeight.Medium,
            )
            if (approval.reason.isNotBlank()) {
                Text("Why: ${approval.reason}", style = MaterialTheme.typography.bodySmall)
            }
            Text("Exact invocation", style = MaterialTheme.typography.labelSmall)
            Text(approval.redactedArguments, style = MaterialTheme.typography.bodyMedium)
            Text("Expires: ${approval.expiresLabel}", style = MaterialTheme.typography.labelSmall)
            if (approval.status == ApprovalStatus.PENDING) {
                Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                    OutlinedButton(
                        onClick = { onAction(VeqriAction.ResolveApproval(approval.id, false)) },
                        modifier = Modifier.weight(1f),
                    ) { Text("Deny") }
                    Button(
                        onClick = { onAction(VeqriAction.ResolveApproval(approval.id, true)) },
                        modifier = Modifier.weight(1f),
                    ) { Text("Approve once") }
                }
            } else {
                Text(approval.status.name.lowercase(), fontWeight = FontWeight.SemiBold)
            }
        }
    }
}

@Composable
private fun CallScreen(
    state: VeqriUiState,
    padding: PaddingValues,
    onAction: (VeqriAction) -> Unit,
) {
    val session = state.voiceSession
    Column(
        modifier = Modifier.fillMaxSize().padding(padding).verticalScroll(rememberScrollState()).padding(16.dp),
        horizontalAlignment = Alignment.CenterHorizontally,
        verticalArrangement = Arrangement.spacedBy(14.dp),
    ) {
        ScreenHeading("Voice session", "Application-owned call UI")
        if (session == null || session.phase == DialogPhase.ENDED) {
            EmptyState("No active call", "Start a local voice session or wait for Veqri Core to call this device.")
            Button(onClick = { onAction(VeqriAction.StartCall) }, modifier = Modifier.fillMaxWidth()) {
                Text("Start Veqri call")
            }
            if (state.isLocalSimulator) {
                OutlinedButton(
                    onClick = { onAction(VeqriAction.SimulateIncomingCall) },
                    modifier = Modifier.fillMaxWidth(),
                ) { Text("Simulate incoming call") }
            }
            return@Column
        }
        Text(session.phaseLabel, style = MaterialTheme.typography.headlineMedium, fontWeight = FontWeight.Bold)
        Text(session.durationLabel, style = MaterialTheme.typography.titleLarge)
        if (session.isSimulatedMedia) SimulatorNotice(session.mediaNotice)
        if (session.phase == DialogPhase.RINGING) {
            Text("Incoming Veqri call")
            Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                OutlinedButton(
                    onClick = { onAction(VeqriAction.DeclineCall(session.id)) },
                    modifier = Modifier.weight(1f),
                ) { Text("Decline") }
                Button(
                    onClick = { onAction(VeqriAction.AnswerCall(session.id)) },
                    modifier = Modifier.weight(1f),
                ) { Text("Answer") }
            }
            return@Column
        }
        OutlinedCard(Modifier.fillMaxWidth()) {
            Column(Modifier.padding(14.dp), verticalArrangement = Arrangement.spacedBy(6.dp)) {
                Text("Live transcript", fontWeight = FontWeight.SemiBold)
                Text(
                    state.finalTranscript.ifBlank {
                        state.partialTranscript.ifBlank { "Waiting for speech…" }
                    },
                )
                Text("Active delegated tasks: ${state.activeTaskCount}")
                Text("TTS: ${session.ttsPlayback.name.lowercase()}")
            }
        }
        Text("Audio route", fontWeight = FontWeight.SemiBold)
        Row(horizontalArrangement = Arrangement.spacedBy(6.dp)) {
            session.availableAudioRoutes.forEach { route ->
                FilterChip(
                    selected = route == session.selectedAudioRoute,
                    onClick = { onAction(VeqriAction.SelectAudioRoute(session.id, route)) },
                    label = { Text(route.label()) },
                )
            }
        }
        Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            FilterChip(
                selected = session.isMuted,
                onClick = { onAction(VeqriAction.ToggleMute(session.id)) },
                label = { Text(if (session.isMuted) "Muted" else "Microphone") },
                modifier = Modifier.weight(1f),
            )
            FilterChip(
                selected = session.isPushToTalk,
                onClick = { onAction(VeqriAction.SetPushToTalk(session.id, !session.isPushToTalk)) },
                label = { Text("Push to talk") },
                modifier = Modifier.weight(1f),
            )
        }
        if (session.isPushToTalk) {
            PushToTalkButton(
                sessionId = session.id,
                isTransmitting = !session.isMuted,
                onPressedChanged = { pressed ->
                    onAction(VeqriAction.SetPushToTalkPressed(session.id, pressed))
                },
            )
        }
        Button(
            onClick = { onAction(VeqriAction.InterruptTts(session.id)) },
            enabled = session.ttsPlayback in setOf(TtsPlaybackStatus.BUFFERING, TtsPlaybackStatus.SPEAKING),
            modifier = Modifier.fillMaxWidth(),
        ) { Text("Interrupt Veqri") }
        OutlinedButton(
            onClick = { onAction(VeqriAction.EndCall(session.id)) },
            modifier = Modifier.fillMaxWidth(),
        ) { Text("End call") }
    }
}

@Composable
private fun PushToTalkButton(
    sessionId: String,
    isTransmitting: Boolean,
    onPressedChanged: (Boolean) -> Unit,
) {
    Surface(
        color = if (isTransmitting) {
            MaterialTheme.colorScheme.errorContainer
        } else {
            MaterialTheme.colorScheme.primaryContainer
        },
        shape = MaterialTheme.shapes.large,
        modifier = Modifier
            .fillMaxWidth()
            .semantics {
                role = Role.Button
                contentDescription = "Hold to talk"
                onClick(label = "Toggle push to talk") {
                    onPressedChanged(!isTransmitting)
                    true
                }
            }
            .pointerInput(sessionId, onPressedChanged) {
                detectTapGestures(
                    onPress = {
                        onPressedChanged(true)
                        try {
                            awaitRelease()
                        } finally {
                            onPressedChanged(false)
                        }
                    },
                )
            },
    ) {
        Text(
            text = if (isTransmitting) "Talking… release to mute" else "Hold to talk",
            modifier = Modifier.padding(18.dp),
            textAlign = androidx.compose.ui.text.style.TextAlign.Center,
            fontWeight = FontWeight.Bold,
        )
    }
}

@Composable
private fun ScreenHeading(title: String, detail: String) {
    Column(Modifier.fillMaxWidth()) {
        Text(title, style = MaterialTheme.typography.headlineSmall, fontWeight = FontWeight.Bold)
        Text(detail, color = MaterialTheme.colorScheme.onSurfaceVariant)
    }
}

@Composable
private fun EmptyState(title: String, detail: String) {
    OutlinedCard(Modifier.fillMaxWidth()) {
        Column(Modifier.padding(20.dp), verticalArrangement = Arrangement.spacedBy(6.dp)) {
            Text(title, style = MaterialTheme.typography.titleMedium, fontWeight = FontWeight.SemiBold)
            Text(detail, color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
    }
}

@Composable
private fun SimulatorNotice(text: String) {
    Card(
        modifier = Modifier.fillMaxWidth(),
        colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.secondaryContainer),
    ) {
        Text(text, modifier = Modifier.padding(12.dp), style = MaterialTheme.typography.bodySmall)
    }
}

private fun AudioRouteKind.label(): String = when (this) {
    AudioRouteKind.EARPIECE -> "Earpiece"
    AudioRouteKind.SPEAKER -> "Speaker"
    AudioRouteKind.WIRED_HEADSET -> "Wired"
    AudioRouteKind.BLUETOOTH -> "Bluetooth"
}
