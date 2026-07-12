package com.veqri.android

import android.Manifest
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.os.Build
import android.os.Bundle
import android.view.WindowManager
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import androidx.activity.result.contract.ActivityResultContracts
import androidx.activity.viewModels
import androidx.core.content.ContextCompat
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import com.veqri.android.ui.AppDestination
import com.veqri.android.ui.VeqriAction
import com.veqri.android.ui.VeqriApp
import com.veqri.android.ui.VeqriViewModel
import com.veqri.android.ui.theme.VeqriTheme

class MainActivity : ComponentActivity() {
    private val viewModel: VeqriViewModel by viewModels {
        VeqriViewModel.Factory((application as VeqriApplication).container.repository)
    }
    private var actionWaitingForPermission: VeqriAction? = null
    private var actionWaitingForVisibleActivity: VeqriAction? = null
    private val permissionLauncher = registerForActivityResult(
        ActivityResultContracts.RequestMultiplePermissions(),
    ) { results ->
        val microphoneGranted = results[Manifest.permission.RECORD_AUDIO] == true ||
            ContextCompat.checkSelfPermission(this, Manifest.permission.RECORD_AUDIO) ==
            PackageManager.PERMISSION_GRANTED
        val pending = actionWaitingForPermission
        actionWaitingForPermission = null
        if (microphoneGranted && pending != null) viewModel.dispatch(pending)
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        configureIncomingCallWindow()
        enableEdgeToEdge()
        setContent {
            val uiStateValue = viewModel.uiState.collectAsStateWithLifecycle().value
            VeqriTheme {
                VeqriApp(
                    state = uiStateValue,
                    onAction = ::handleUiAction,
                )
            }
        }
        readIntent(intent)
    }

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        setIntent(intent)
        readIntent(intent)
    }

    override fun onPostResume() {
        super.onPostResume()
        actionWaitingForVisibleActivity?.let { action ->
            actionWaitingForVisibleActivity = null
            requestCallPermissionsThen(action)
        }
    }

    private fun handleUiAction(action: VeqriAction) {
        when (action) {
            VeqriAction.StartCall,
            is VeqriAction.AnswerCall,
            -> requestCallPermissionsThen(action)
            else -> viewModel.dispatch(action)
        }
    }

    private fun requestCallPermissionsThen(action: VeqriAction) {
        if (ContextCompat.checkSelfPermission(this, Manifest.permission.RECORD_AUDIO) ==
            PackageManager.PERMISSION_GRANTED
        ) {
            viewModel.dispatch(action)
            return
        }
        actionWaitingForPermission = action
        permissionLauncher.launch(requiredCallPermissions())
    }

    private fun requiredCallPermissions(): Array<String> = buildList {
        add(Manifest.permission.RECORD_AUDIO)
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
            add(Manifest.permission.POST_NOTIFICATIONS)
        }
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S) {
            add(Manifest.permission.BLUETOOTH_CONNECT)
        }
    }.toTypedArray()

    private fun readIntent(intent: Intent?) {
        val sessionId = intent?.getStringExtra(EXTRA_SESSION_ID)
        when (intent?.action) {
            ACTION_ANSWER_CALL -> if (sessionId != null) {
                // Defer until onPostResume so a visible activity owns microphone FGS startup.
                actionWaitingForVisibleActivity = VeqriAction.AnswerCall(sessionId)
            }
            ACTION_SHOW_CALL -> viewModel.dispatch(VeqriAction.Navigate(AppDestination.CALL))
        }
    }

    @Suppress("DEPRECATION")
    private fun configureIncomingCallWindow() {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O_MR1) {
            setShowWhenLocked(true)
            setTurnScreenOn(true)
        } else {
            window.addFlags(
                WindowManager.LayoutParams.FLAG_SHOW_WHEN_LOCKED or
                    WindowManager.LayoutParams.FLAG_TURN_SCREEN_ON,
            )
        }
    }

    companion object {
        private const val ACTION_SHOW_CALL = "com.veqri.android.action.SHOW_CALL"
        private const val ACTION_ANSWER_CALL = "com.veqri.android.action.ANSWER_IN_UI"
        private const val EXTRA_SESSION_ID = "session_id"

        fun intentForCall(context: Context, sessionId: String): Intent =
            Intent(context, MainActivity::class.java)
                .setAction(ACTION_SHOW_CALL)
                .putExtra(EXTRA_SESSION_ID, sessionId)
                .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK or Intent.FLAG_ACTIVITY_SINGLE_TOP)

        fun intentForAnswer(context: Context, sessionId: String): Intent =
            Intent(context, MainActivity::class.java)
                .setAction(ACTION_ANSWER_CALL)
                .putExtra(EXTRA_SESSION_ID, sessionId)
                .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK or Intent.FLAG_ACTIVITY_SINGLE_TOP)
    }
}
