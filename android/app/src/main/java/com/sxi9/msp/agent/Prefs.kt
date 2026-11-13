package com.sxi9.msp.agent

import android.content.Context
import android.content.SharedPreferences

/** App-private storage for the enrollment state. Keystore wrapping is phase 2. */
class Prefs(context: Context) {
    private val p: SharedPreferences =
        context.applicationContext.getSharedPreferences("msp_agent", Context.MODE_PRIVATE)

    var serverUrl: String
        get() = p.getString("server_url", "") ?: ""
        set(v) = p.edit().putString("server_url", v).apply()

    var deviceToken: String
        get() = p.getString("device_token", "") ?: ""
        set(v) = p.edit().putString("device_token", v).apply()

    var deviceId: Long
        get() = p.getLong("device_id", 0L)
        set(v) = p.edit().putLong("device_id", v).apply()

    var intervalS: Int
        get() = p.getInt("interval_s", INTERVAL_S)
        set(v) = p.edit().putInt("interval_s", v).apply()

    /** Human-readable outcome of the last sync, shown on the enrollment screen. */
    var lastSync: String
        get() = p.getString("last_sync", "") ?: ""
        set(v) = p.edit().putString("last_sync", v).apply()

    val enrolled: Boolean get() = deviceToken.isNotEmpty() && serverUrl.isNotEmpty()

    fun saveEnrollment(serverUrl: String, r: EnrollResult) {
        p.edit()
            .putString("server_url", serverUrl)
            .putString("device_token", r.deviceToken)
            .putLong("device_id", r.deviceId)
            .putInt("interval_s", INTERVAL_S)
            .putString("last_sync", "")
            .apply()
    }
}
