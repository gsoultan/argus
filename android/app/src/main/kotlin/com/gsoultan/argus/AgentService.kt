package com.gsoultan.argus

import android.app.Notification
import android.app.PendingIntent
import android.app.Service
import android.content.Context
import android.content.Intent
import android.content.pm.ServiceInfo
import android.content.res.Configuration
import android.media.projection.MediaProjection
import android.media.projection.MediaProjectionManager
import android.os.Build
import android.os.IBinder
import android.util.Log
import androidx.core.app.NotificationCompat

/**
 * The agent as a foreground service: it holds the tunnel to Argus while the
 * app is linked, and captures the screen while a session is open.
 *
 * Android 14 ties screen capture to a foreground service of type
 * mediaProjection, started before the projection is used and ended with
 * it, so the capture lives here and the notification says what is going on.
 */
class AgentService : Service() {
    private val app get() = application as ArgusApp
    private var capture: ScreenCapture? = null
    private var projection: MediaProjection? = null

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        when (intent?.action) {
            ACTION_STOP -> {
                stopCapture()
                app.agent.stop()
                stopForeground(STOP_FOREGROUND_REMOVE)
                stopSelf()
                return START_NOT_STICKY
            }
            ACTION_END_SESSIONS -> {
                app.agent.endSessions()
                return START_STICKY
            }
            ACTION_CAPTURE -> {
                val data = intent.getParcelableExtra<Intent>(EXTRA_PROJECTION)
                if (data != null) startCapture(data)
                return START_STICKY
            }
            ACTION_STOP_CAPTURE -> {
                stopCapture()
                return START_STICKY
            }
        }
        foreground(false)
        try {
            app.agent.start()
        } catch (e: Exception) {
            Log.e(ArgusApp.TAG, "agent did not start: ${e.message}")
            app.note("error ${e.message}")
            stopForeground(STOP_FOREGROUND_REMOVE)
            stopSelf()
            return START_NOT_STICKY
        }
        instance = this
        // A device set up for unattended arms itself: the helper is started
        // over ADB and the agent told it is unattended, so a session opens
        // with nobody there. Off the main thread, since arming talks to adbd.
        if (app.unattended.paired() && !app.unattended.armed) {
            Thread {
                app.unattended.arm()?.let { app.note("unattended: $it") }
            }.start()
        }
        return START_STICKY
    }

    override fun onDestroy() {
        stopCapture()
        instance = null
        super.onDestroy()
    }

    override fun onConfigurationChanged(newConfig: Configuration) {
        super.onConfigurationChanged(newConfig)
        // Rotation: the capture is sized to the screen, so start it over.
        capture?.resize()
    }

    /** Shows what the service is doing: connected, or who is on the device. */
    fun foreground(capturing: Boolean) {
        val open = app.agent.sessions() > 0
        val b = NotificationCompat.Builder(this, if (open) ArgusApp.CHANNEL_SESSION else ArgusApp.CHANNEL_STATUS)
            .setSmallIcon(R.drawable.ic_notification)
            .setOngoing(true)
            .setContentIntent(
                PendingIntent.getActivity(
                    this, 0, Intent(this, MainActivity::class.java), PendingIntent.FLAG_IMMUTABLE,
                ),
            )
        if (open) {
            b.setContentTitle(getString(R.string.notif_session, app.bridge.who()))
                .setPriority(NotificationCompat.PRIORITY_HIGH)
                .setCategory(Notification.CATEGORY_SERVICE)
                .addAction(
                    0, getString(R.string.notif_stop),
                    PendingIntent.getService(
                        this, 1, Intent(this, AgentService::class.java).setAction(ACTION_END_SESSIONS),
                        PendingIntent.FLAG_IMMUTABLE,
                    ),
                )
        } else {
            b.setContentTitle(getString(if (app.agent.connected()) R.string.notif_connected else R.string.notif_connecting))
                .setPriority(NotificationCompat.PRIORITY_LOW)
        }
        // Android 14 wants a type for every foreground service; before it,
        // specialUse does not exist and none is fine.
        var type = if (Build.VERSION.SDK_INT >= 34) ServiceInfo.FOREGROUND_SERVICE_TYPE_SPECIAL_USE else 0
        if (capturing) type = type or ServiceInfo.FOREGROUND_SERVICE_TYPE_MEDIA_PROJECTION
        startForeground(NOTIFICATION_ID, b.build(), type)
    }

    private fun startCapture(data: Intent) {
        if (capture != null) return
        // The service must be in the foreground as mediaProjection before
        // the projection is taken, or Android 14 refuses it.
        foreground(true)
        val mpm = getSystemService(Context.MEDIA_PROJECTION_SERVICE) as MediaProjectionManager
        val p = mpm.getMediaProjection(android.app.Activity.RESULT_OK, data) ?: return
        p.registerCallback(object : MediaProjection.Callback() {
            override fun onStop() {
                // The holder stopped it from the system's own control: end
                // the sessions, which is what they meant.
                app.agent.endSessions()
                stopCapture()
            }
        }, null)
        projection = p
        capture = ScreenCapture(this, p, app.agent).also { it.start() }
        app.bridge.captureStarted(capture!!)
    }

    private fun stopCapture() {
        capture?.stop()
        capture = null
        projection?.stop()
        projection = null
        app.bridge.captureStarted(null)
        if (instance != null) foreground(false)
    }

    companion object {
        const val NOTIFICATION_ID = 1
        const val ACTION_STOP = "com.gsoultan.argus.STOP"
        const val ACTION_END_SESSIONS = "com.gsoultan.argus.END_SESSIONS"
        const val ACTION_CAPTURE = "com.gsoultan.argus.CAPTURE"
        const val ACTION_STOP_CAPTURE = "com.gsoultan.argus.STOP_CAPTURE"
        const val EXTRA_PROJECTION = "projection"

        @Volatile
        var instance: AgentService? = null
            private set

        fun start(ctx: Context) {
            ctx.startForegroundService(Intent(ctx, AgentService::class.java))
        }

        fun stop(ctx: Context) {
            ctx.startService(Intent(ctx, AgentService::class.java).setAction(ACTION_STOP))
        }

        fun capture(ctx: Context, projection: Intent) {
            ctx.startForegroundService(
                Intent(ctx, AgentService::class.java).setAction(ACTION_CAPTURE).putExtra(EXTRA_PROJECTION, projection),
            )
        }

        fun stopCapture(ctx: Context) {
            ctx.startService(Intent(ctx, AgentService::class.java).setAction(ACTION_STOP_CAPTURE))
        }
    }
}
