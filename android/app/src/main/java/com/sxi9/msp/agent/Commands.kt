package com.sxi9.msp.agent

import android.app.Notification
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.admin.DevicePolicyManager
import android.content.Context
import android.content.Intent
import java.time.Instant
import java.time.temporal.ChronoUnit

/**
 * The built-in command kinds Android supports: lock, ping, message (A1). Anything else is
 * rejected with exit_code -1 and error "unsupported kind: X". Commands arrive only in this
 * device's own sync response (D5); nothing here ever runs free text.
 */
object Commands {
    const val MESSAGE_CHANNEL = "msp_message"

    fun run(ctx: Context, cmd: Command): CommandResult {
        val started = now()
        val r: Triple<Int, String, String> = try {
            when (cmd.kind) {
                "ping" -> Triple(0, "pong", "")
                "lock" -> lock(ctx)
                "message" -> message(ctx, cmd)
                else -> Triple(-1, "", "unsupported kind: ${cmd.kind}")
            }
        } catch (e: Exception) {
            Triple(1, "", "${cmd.kind}: ${e.message ?: e.javaClass.simpleName}")
        }
        return CommandResult(
            exitCode = r.first,
            stdout = r.second,
            stderr = "",
            startedAt = started,
            finishedAt = now(),
            error = r.third,
        )
    }

    private fun lock(ctx: Context): Triple<Int, String, String> {
        if (!AdminReceiver.isActive(ctx)) return Triple(1, "", "device admin not granted")
        val dpm = ctx.getSystemService(Context.DEVICE_POLICY_SERVICE) as DevicePolicyManager
        dpm.lockNow()
        return Triple(0, "locked", "")
    }

    private fun message(ctx: Context, cmd: Command): Triple<Int, String, String> {
        val nm = ctx.getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager
        if (!nm.areNotificationsEnabled()) return Triple(1, "", "notifications disabled")
        val open = PendingIntent.getActivity(
            ctx, 0, Intent(ctx, MainActivity::class.java), PendingIntent.FLAG_IMMUTABLE
        )
        val n = Notification.Builder(ctx, MESSAGE_CHANNEL)
            .setSmallIcon(R.drawable.ic_notification)
            .setContentTitle(ctx.getString(R.string.notification_title))
            .setContentText(cmd.text)
            .setStyle(Notification.BigTextStyle().bigText(cmd.text))
            .setContentIntent(open)
            .setAutoCancel(true)
            .build()
        // One notification per command id so repeated messages do not replace each other.
        nm.notify(1000 + (cmd.id % 100000).toInt(), n)
        return Triple(0, "shown", "")
    }

    private fun now(): String = Instant.now().truncatedTo(ChronoUnit.SECONDS).toString()
}
