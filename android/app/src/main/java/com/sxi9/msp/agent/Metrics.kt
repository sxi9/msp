package com.sxi9.msp.agent

import android.app.ActivityManager
import android.content.Context
import android.os.BatteryManager
import android.os.Build
import android.os.Environment
import android.os.StatFs
import android.os.SystemClock
import android.provider.Settings
import org.json.JSONArray
import org.json.JSONObject
import java.net.Inet4Address
import java.net.NetworkInterface

/**
 * Inventory and health readings in the shapes of enroll_android.json / sync_android.json;
 * facts also carry the installed-apps inventory (sync_android_apps.json).
 */
object Metrics {
    private const val MB = 1024L * 1024L
    private const val GB = 1024.0 * 1024.0 * 1024.0

    fun deviceUid(ctx: Context): String =
        Settings.Secure.getString(ctx.contentResolver, Settings.Secure.ANDROID_ID) ?: ""

    fun hostname(ctx: Context): String =
        Settings.Global.getString(ctx.contentResolver, "device_name")?.takeIf { it.isNotBlank() }
            ?: Build.MODEL

    fun arch(): String = Protocol.archOf(Build.SUPPORTED_ABIS.firstOrNull() ?: "")

    fun osVersion(): String = "Android ${Build.VERSION.RELEASE} (${Build.VERSION.SDK_INT})"

    fun uptimeS(): Long = SystemClock.elapsedRealtime() / 1000L

    fun facts(ctx: Context): JSONObject = JSONObject()
        .put("kernel", System.getProperty("os.version") ?: "")
        .put("cpu_model", Build.HARDWARE)
        .put("cpu_count", Runtime.getRuntime().availableProcessors())
        .put("mem_total_mb", memory(ctx).totalMem / MB)
        .put("manufacturer", Build.MANUFACTURER)
        .put("model", Build.MODEL)
        .put("serial", "")
        .put("ips", ips())
        // Always present: an empty inventory (or a failed PackageManager read) is sent as
        // "apps":[], which the server stores as no key (docs/PROTOCOL.md 3.2a).
        .put("apps", Apps.toJson(AppInventory.collect(ctx)))

    fun metrics(ctx: Context): JSONObject {
        val mem = memory(ctx)
        val data = StatFs(Environment.getDataDirectory().path)
        val totalGb = data.totalBytes / GB
        val usedGb = (data.totalBytes - data.availableBytes) / GB
        val disk = JSONObject()
            .put("mount", "/data")
            .put("total_gb", round1(totalGb))
            .put("used_gb", round1(usedGb))
        val o = JSONObject()
            .put("cpu_pct", JSONObject.NULL) // Android apps cannot read /proc/stat
            .put("mem_total_mb", mem.totalMem / MB)
            .put("mem_used_mb", (mem.totalMem - mem.availMem) / MB)
            .put("disks", JSONArray().put(disk))
        val bm = ctx.getSystemService(Context.BATTERY_SERVICE) as? BatteryManager
        val pct = bm?.getIntProperty(BatteryManager.BATTERY_PROPERTY_CAPACITY) ?: Int.MIN_VALUE
        if (bm != null && pct in 0..100) {
            o.put("battery_pct", pct).put("battery_charging", bm.isCharging)
        }
        return o.put("ips", ips())
    }

    private fun memory(ctx: Context): ActivityManager.MemoryInfo {
        val info = ActivityManager.MemoryInfo()
        (ctx.getSystemService(Context.ACTIVITY_SERVICE) as ActivityManager).getMemoryInfo(info)
        return info
    }

    /** Non-loopback IPv4 addresses of up interfaces. */
    private fun ips(): JSONArray {
        val out = JSONArray()
        try {
            val ifaces = NetworkInterface.getNetworkInterfaces() ?: return out
            for (iface in ifaces) {
                if (!iface.isUp || iface.isLoopback) continue
                for (addr in iface.inetAddresses) {
                    if (addr is Inet4Address && !addr.isLoopbackAddress) {
                        out.put(addr.hostAddress)
                    }
                }
            }
        } catch (e: Exception) {
            // No network permission issue on Android; a failure here just means no IPs reported.
        }
        return out
    }

    private fun round1(v: Double): Double = Math.round(v * 10.0) / 10.0
}
