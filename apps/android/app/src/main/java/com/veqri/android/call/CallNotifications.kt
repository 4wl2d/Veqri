package com.veqri.android.call

import android.Manifest
import android.annotation.SuppressLint
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.os.Build
import androidx.core.app.NotificationCompat
import androidx.core.app.NotificationManagerCompat
import androidx.core.content.ContextCompat
import com.veqri.android.MainActivity
import com.veqri.android.R
import com.veqri.android.VeqriApplication
import com.veqri.android.data.ApprovalRequest
import com.veqri.android.data.DialogPhase
import com.veqri.android.data.TaskRecord
import com.veqri.android.data.VoiceSession
import kotlinx.coroutines.launch

interface CallLifecycleController {
    fun publishIncomingCall(session: VoiceSession)
    fun updateActiveCall(session: VoiceSession)
    fun endActiveCall()
    fun publishTaskCompleted(task: TaskRecord)
    fun publishApprovalRequired(approval: ApprovalRequest)
}

class AndroidCallLifecycleController(context: Context) : CallLifecycleController {
    private val appContext = context.applicationContext
    private val notifications = NotificationManagerCompat.from(appContext)

    init {
        createChannels()
    }

    override fun publishIncomingCall(session: VoiceSession) {
        if (!canPostNotifications()) return
        val contentIntent = PendingIntent.getActivity(
            appContext,
            session.id.hashCode(),
            MainActivity.intentForCall(appContext, session.id),
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
        )
        val answerIntent = PendingIntent.getActivity(
            appContext,
            session.id.hashCode() + 1,
            MainActivity.intentForAnswer(appContext, session.id),
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
        )
        val declineIntent = actionPendingIntent(CallActionReceiver.ACTION_DECLINE, session.id, 2)
        val notification = NotificationCompat.Builder(appContext, CALL_CHANNEL_ID)
            .setSmallIcon(android.R.drawable.sym_call_incoming)
            .setContentTitle("Incoming Veqri call")
            .setContentText("Your local Veqri Core is requesting a voice session.")
            .setCategory(NotificationCompat.CATEGORY_CALL)
            .setPriority(NotificationCompat.PRIORITY_MAX)
            .setOngoing(true)
            .setAutoCancel(false)
            .setContentIntent(contentIntent)
            .setFullScreenIntent(contentIntent, canUseFullScreenIntent())
            .addAction(0, "Decline", declineIntent)
            .addAction(0, "Answer", answerIntent)
            .build()
        notifySafely(INCOMING_CALL_NOTIFICATION_ID, notification)
    }

    override fun updateActiveCall(session: VoiceSession) {
        notifications.cancel(INCOMING_CALL_NOTIFICATION_ID)
        if (session.phase == DialogPhase.RINGING || session.phase == DialogPhase.ENDED) return
        CallForegroundService.start(appContext, session)
    }

    override fun endActiveCall() {
        notifications.cancel(INCOMING_CALL_NOTIFICATION_ID)
        CallForegroundService.stop(appContext)
    }

    override fun publishTaskCompleted(task: TaskRecord) {
        if (!canPostNotifications()) return
        val notification = NotificationCompat.Builder(appContext, EVENT_CHANNEL_ID)
            .setSmallIcon(android.R.drawable.stat_notify_chat)
            .setContentTitle("Veqri task completed")
            .setContentText(task.summary.ifBlank { task.goal })
            .setAutoCancel(true)
            .setContentIntent(mainPendingIntent(task.id.hashCode()))
            .build()
        notifySafely(task.id.hashCode(), notification)
    }

    override fun publishApprovalRequired(approval: ApprovalRequest) {
        if (!canPostNotifications()) return
        val notification = NotificationCompat.Builder(appContext, EVENT_CHANNEL_ID)
            .setSmallIcon(android.R.drawable.ic_dialog_alert)
            .setContentTitle("Veqri needs approval")
            .setContentText(approval.title)
            .setPriority(NotificationCompat.PRIORITY_HIGH)
            .setAutoCancel(true)
            .setContentIntent(mainPendingIntent(approval.id.hashCode()))
            .build()
        notifySafely(approval.id.hashCode(), notification)
    }

    private fun mainPendingIntent(requestCode: Int): PendingIntent = PendingIntent.getActivity(
        appContext,
        requestCode,
        Intent(appContext, MainActivity::class.java),
        PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
    )

    private fun actionPendingIntent(action: String, sessionId: String, offset: Int): PendingIntent =
        PendingIntent.getBroadcast(
            appContext,
            sessionId.hashCode() + offset,
            Intent(appContext, CallActionReceiver::class.java)
                .setAction(action)
                .putExtra(CallActionReceiver.EXTRA_SESSION_ID, sessionId),
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
        )

    private fun canPostNotifications(): Boolean =
        Build.VERSION.SDK_INT < Build.VERSION_CODES.TIRAMISU ||
            ContextCompat.checkSelfPermission(appContext, Manifest.permission.POST_NOTIFICATIONS) ==
            PackageManager.PERMISSION_GRANTED

    private fun canUseFullScreenIntent(): Boolean =
        Build.VERSION.SDK_INT < Build.VERSION_CODES.UPSIDE_DOWN_CAKE ||
            appContext.getSystemService(NotificationManager::class.java).canUseFullScreenIntent()

    @SuppressLint("MissingPermission")
    private fun notifySafely(id: Int, notification: android.app.Notification) {
        if (!canPostNotifications()) return
        try {
            notifications.notify(id, notification)
        } catch (_: SecurityException) {
            // Permission can be revoked between the explicit check and the framework call.
        }
    }

    private fun createChannels() {
        val manager = appContext.getSystemService(NotificationManager::class.java)
        manager.createNotificationChannels(
            listOf(
                NotificationChannel(
                    CALL_CHANNEL_ID,
                    appContext.getString(R.string.call_channel_name),
                    NotificationManager.IMPORTANCE_HIGH,
                ).apply {
                    description = "Incoming and active Veqri voice sessions"
                    lockscreenVisibility = NotificationCompat.VISIBILITY_PRIVATE
                },
                NotificationChannel(
                    EVENT_CHANNEL_ID,
                    appContext.getString(R.string.event_channel_name),
                    NotificationManager.IMPORTANCE_DEFAULT,
                ).apply {
                    description = "Task progress, completion, and approval requests"
                    lockscreenVisibility = NotificationCompat.VISIBILITY_PRIVATE
                },
            ),
        )
    }

    companion object {
        const val CALL_CHANNEL_ID = "veqri.calls"
        const val EVENT_CHANNEL_ID = "veqri.events"
        const val INCOMING_CALL_NOTIFICATION_ID = 4_210
    }
}

class CallActionReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent) {
        val sessionId = intent.getStringExtra(EXTRA_SESSION_ID) ?: return
        val pendingResult = goAsync()
        val application = context.applicationContext as VeqriApplication
        application.appScope.launch {
            try {
                when (intent.action) {
                    ACTION_ANSWER -> application.container.repository.answerCall(sessionId)
                    ACTION_DECLINE -> application.container.repository.declineCall(sessionId)
                    ACTION_END -> application.container.repository.endCall(sessionId)
                    ACTION_TOGGLE_MUTE -> application.container.repository.toggleMute(sessionId)
                }
            } finally {
                pendingResult.finish()
            }
        }
    }

    companion object {
        const val ACTION_ANSWER = "com.veqri.android.action.ANSWER"
        const val ACTION_DECLINE = "com.veqri.android.action.DECLINE"
        const val ACTION_END = "com.veqri.android.action.END"
        const val ACTION_TOGGLE_MUTE = "com.veqri.android.action.TOGGLE_MUTE"
        const val EXTRA_SESSION_ID = "session_id"
    }
}
