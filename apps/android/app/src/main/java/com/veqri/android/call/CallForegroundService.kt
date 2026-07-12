package com.veqri.android.call

import android.app.PendingIntent
import android.app.Service
import android.content.Context
import android.content.Intent
import android.os.IBinder
import androidx.core.app.NotificationCompat
import androidx.core.content.ContextCompat
import com.veqri.android.MainActivity
import com.veqri.android.data.VoiceSession

class CallForegroundService : Service() {
    override fun onCreate() {
        super.onCreate()
        // Channel creation is centralized in AndroidCallLifecycleController.
        AndroidCallLifecycleController(applicationContext)
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        if (intent?.action == ACTION_STOP) {
            stopForeground(STOP_FOREGROUND_REMOVE)
            stopSelf()
            return START_NOT_STICKY
        }
        val sessionId = intent?.getStringExtra(EXTRA_SESSION_ID) ?: return START_NOT_STICKY
        val phase = intent.getStringExtra(EXTRA_PHASE).orEmpty()
        startForeground(NOTIFICATION_ID, activeCallNotification(sessionId, phase))
        return START_STICKY
    }

    override fun onBind(intent: Intent?): IBinder? = null

    private fun activeCallNotification(sessionId: String, phase: String) =
        NotificationCompat.Builder(this, AndroidCallLifecycleController.CALL_CHANNEL_ID)
            .setSmallIcon(android.R.drawable.sym_call_outgoing)
            .setContentTitle("Veqri call in progress")
            .setContentText(phase.lowercase().replace('_', ' ').ifBlank { "connected" })
            .setCategory(NotificationCompat.CATEGORY_CALL)
            .setPriority(NotificationCompat.PRIORITY_HIGH)
            .setOngoing(true)
            .setOnlyAlertOnce(true)
            .setContentIntent(
                PendingIntent.getActivity(
                    this,
                    sessionId.hashCode(),
                    MainActivity.intentForCall(this, sessionId),
                    PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
                ),
            )
            .addAction(
                0,
                "Mute",
                callAction(CallActionReceiver.ACTION_TOGGLE_MUTE, sessionId, 1),
            )
            .addAction(0, "End", callAction(CallActionReceiver.ACTION_END, sessionId, 2))
            .build()

    private fun callAction(action: String, sessionId: String, offset: Int): PendingIntent =
        PendingIntent.getBroadcast(
            this,
            sessionId.hashCode() + offset,
            Intent(this, CallActionReceiver::class.java)
                .setAction(action)
                .putExtra(CallActionReceiver.EXTRA_SESSION_ID, sessionId),
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
        )

    companion object {
        private const val ACTION_START = "com.veqri.android.call.START"
        private const val ACTION_STOP = "com.veqri.android.call.STOP"
        private const val EXTRA_SESSION_ID = "session_id"
        private const val EXTRA_PHASE = "phase"
        private const val NOTIFICATION_ID = 4_211

        fun start(context: Context, session: VoiceSession) {
            val intent = Intent(context, CallForegroundService::class.java)
                .setAction(ACTION_START)
                .putExtra(EXTRA_SESSION_ID, session.id)
                .putExtra(EXTRA_PHASE, session.phase.name)
            ContextCompat.startForegroundService(context, intent)
        }

        fun stop(context: Context) {
            context.stopService(Intent(context, CallForegroundService::class.java))
        }
    }
}
