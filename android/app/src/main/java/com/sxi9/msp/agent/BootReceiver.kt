package com.sxi9.msp.agent

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.util.Log

/** Restarts the agent after a reboot when the device is enrolled. */
class BootReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent) {
        if (intent.action != Intent.ACTION_BOOT_COMPLETED) return
        if (!Prefs(context).enrolled) return
        try {
            AgentService.start(context)
        } catch (e: Exception) {
            Log.w("msp-agent", "boot start failed: ${e.message}")
        }
    }
}
