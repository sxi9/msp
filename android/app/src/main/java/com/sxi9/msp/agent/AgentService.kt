package com.sxi9.msp.agent

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Context
import android.content.Intent
import android.content.pm.ServiceInfo
import android.os.Build
import android.os.Handler
import android.os.HandlerThread
import android.os.IBinder
import android.util.Log
import java.io.IOException
import java.text.SimpleDateFormat
import java.util.Date
import java.util.Locale

/**
 * Foreground service (type specialUse) that syncs every poll_interval_s, processes the
 * commands returned for this device sequentially and posts each result before the next sync.
 */
class AgentService : Service() {
    companion object {
        const val CHANNEL = "msp_agent"
        const val NOTIFICATION_ID = 1
        private const val TAG = "msp-agent"
        // docs/PROTOCOL.md 3.3: facts on the first sync after start and every 120th sync.
        private const val FACTS_EVERY = 120
        private const val BACKOFF_MIN_S = 30
        private const val BACKOFF_MAX_S = 600
        private const val UNAUTHORIZED_RETRY_S = 300

        fun start(ctx: Context) {
            ctx.startForegroundService(Intent(ctx, AgentService::class.java))
        }

        fun stop(ctx: Context) {
            ctx.stopService(Intent(ctx, AgentService::class.java))
        }
    }

    private lateinit var thread: HandlerThread
    private lateinit var handler: Handler
    private lateinit var prefs: Prefs
    private var syncCount = 0L
    private var backoffS = BACKOFF_MIN_S

    private val syncRunnable = Runnable { syncOnce() }

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onCreate() {
        super.onCreate()
        prefs = Prefs(this)
        createChannels()
        thread = HandlerThread("msp-agent-sync")
        thread.start()
        handler = Handler(thread.looper)
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        val n = notification(getString(R.string.notification_title), "Starting")
        if (Build.VERSION.SDK_INT >= 34) {
            startForeground(NOTIFICATION_ID, n, ServiceInfo.FOREGROUND_SERVICE_TYPE_SPECIAL_USE)
        } else {
            startForeground(NOTIFICATION_ID, n)
        }
        if (!prefs.enrolled) {
            Log.w(TAG, "not enrolled; stopping")
            stopSelf()
            return START_NOT_STICKY
        }
        // Re-run (or run now) regardless of what was scheduled before.
        handler.removeCallbacks(syncRunnable)
        handler.post(syncRunnable)
        return START_STICKY
    }

    override fun onDestroy() {
        handler.removeCallbacks(syncRunnable)
        thread.quitSafely()
        super.onDestroy()
    }

    /** Runs on the HandlerThread. */
    private fun syncOnce() {
        val server = prefs.serverUrl
        val token = prefs.deviceToken
        if (server.isEmpty() || token.isEmpty()) {
            stopSelf()
            return
        }
        val facts = if (syncCount % FACTS_EVERY == 0L) Metrics.facts(this) else null
        syncCount++
        val delayS: Int = try {
            val body = Protocol.buildSyncJson(Metrics.uptimeS(), Metrics.metrics(this), facts)
            val resp = ApiClient.sync(server, token, body)
            backoffS = BACKOFF_MIN_S
            val time = SimpleDateFormat("HH:mm", Locale.getDefault()).format(Date())
            update("Last sync $time", "ok at $time, ${resp.commands.size} command(s)")
            for (cmd in resp.commands) {
                runCommand(server, token, cmd)
            }
            Protocol.clampPoll(resp.pollIntervalS)
        } catch (e: UnauthorizedException) {
            Log.w(TAG, "token rejected: ${e.message}")
            update(getString(R.string.token_rejected), "token rejected (${e.message})")
            UNAUTHORIZED_RETRY_S
        } catch (e: IOException) {
            Log.w(TAG, "sync failed: ${e.message}")
            val wait = backoffS
            backoffS = minOf(backoffS * 2, BACKOFF_MAX_S)
            update("Sync failed, retrying in ${wait}s", "failed: ${e.message}")
            wait
        } catch (e: Exception) {
            Log.e(TAG, "sync error", e)
            update("Sync error", "error: ${e.message}")
            backoffS
        }
        handler.postDelayed(syncRunnable, delayS * 1000L)
    }

    private fun runCommand(server: String, token: String, cmd: Command) {
        val result = Commands.run(this, cmd)
        Log.i(TAG, "command ${cmd.id} ${cmd.kind}: exit ${result.exitCode} ${result.error}")
        try {
            ApiClient.postResult(server, token, cmd.id, Protocol.buildResultJson(result))
        } catch (e: IOException) {
            // The server marks it lost after timeout_s + 5 min; it is never re-run here.
            Log.w(TAG, "result for ${cmd.id} not delivered: ${e.message}")
        }
    }

    private fun update(notificationText: String, lastSync: String) {
        prefs.lastSync = lastSync
        val nm = getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager
        nm.notify(NOTIFICATION_ID, notification(getString(R.string.notification_title), notificationText))
    }

    private fun notification(title: String, text: String): Notification {
        val open = PendingIntent.getActivity(
            this, 0, Intent(this, MainActivity::class.java), PendingIntent.FLAG_IMMUTABLE
        )
        return Notification.Builder(this, CHANNEL)
            .setSmallIcon(R.drawable.ic_notification)
            .setContentTitle(title)
            .setContentText(text)
            .setContentIntent(open)
            .setOngoing(true)
            .setOnlyAlertOnce(true)
            .build()
    }

    private fun createChannels() {
        val nm = getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager
        nm.createNotificationChannel(
            NotificationChannel(CHANNEL, getString(R.string.channel_agent), NotificationManager.IMPORTANCE_LOW)
        )
        nm.createNotificationChannel(
            NotificationChannel(
                Commands.MESSAGE_CHANNEL,
                getString(R.string.channel_message),
                NotificationManager.IMPORTANCE_HIGH
            )
        )
    }
}
