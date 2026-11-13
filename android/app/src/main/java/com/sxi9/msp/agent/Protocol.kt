package com.sxi9.msp.agent

import org.json.JSONArray
import org.json.JSONObject

// Pure Kotlin (no android.* imports): the wire shapes from docs/PROTOCOL.md and
// internal/protocol/fixtures. Exercised by ProtocolTest on the JVM.

const val AGENT_VERSION = "0.1.0"
const val INTERVAL_S = 60
const val OUTPUT_CAP = 65536

data class EnrollResult(val deviceId: Long, val deviceToken: String, val pollIntervalS: Int)

/** One command from a sync response. text is "" unless kind == "message". */
data class Command(val id: Long, val kind: String, val text: String, val timeoutS: Int)

data class SyncResponse(val pollIntervalS: Int, val commands: List<Command>)

data class CommandResult(
    val exitCode: Int,
    val stdout: String,
    val stderr: String,
    val startedAt: String,
    val finishedAt: String,
    val error: String,
)

object Protocol {
    fun buildEnrollJson(
        token: String,
        uid: String,
        hostname: String,
        arch: String,
        osVersion: String,
        facts: JSONObject,
    ): JSONObject = JSONObject()
        .put("enrollment_token", token)
        .put("device_uid", uid)
        .put("hostname", hostname)
        .put("platform", "android")
        .put("arch", arch)
        .put("os_version", osVersion)
        .put("agent_version", AGENT_VERSION)
        .put("interval_s", INTERVAL_S)
        .put("facts", facts)

    /** facts is sent on the first sync after start and periodically; null omits the key. */
    fun buildSyncJson(uptimeS: Long, metrics: JSONObject, facts: JSONObject?): JSONObject {
        val o = JSONObject()
            .put("agent_version", AGENT_VERSION)
            .put("interval_s", INTERVAL_S)
            .put("wait_s", 0)
            .put("uptime_s", uptimeS)
            .put("metrics", metrics)
        if (facts != null) o.put("facts", facts)
        return o
    }

    fun parseEnrollResponse(body: String): EnrollResult {
        val o = JSONObject(body)
        return EnrollResult(
            deviceId = o.getLong("device_id"),
            deviceToken = o.getString("device_token"),
            pollIntervalS = o.getInt("poll_interval_s"),
        )
    }

    /** poll_interval_s of a sync response. */
    fun parseSyncResponse(body: String): Int = JSONObject(body).getInt("poll_interval_s")

    /** Commands of a sync response, in server order. Unknown keys are ignored. */
    fun parseSyncCommands(body: String): List<Command> {
        val arr = JSONObject(body).optJSONArray("commands") ?: JSONArray()
        val out = ArrayList<Command>(arr.length())
        for (i in 0 until arr.length()) {
            val c = arr.getJSONObject(i)
            out.add(
                Command(
                    id = c.getLong("id"),
                    kind = c.getString("kind"),
                    text = c.optString("text", ""),
                    timeoutS = c.optInt("timeout_s", 60),
                )
            )
        }
        return out
    }

    fun parseSync(body: String): SyncResponse =
        SyncResponse(parseSyncResponse(body), parseSyncCommands(body))

    fun buildResultJson(r: CommandResult): JSONObject {
        val stdout = cap(r.stdout)
        val stderr = cap(r.stderr)
        return JSONObject()
            .put("exit_code", r.exitCode)
            .put("stdout", stdout.first)
            .put("stderr", stderr.first)
            .put("started_at", r.startedAt)
            .put("finished_at", r.finishedAt)
            .put("truncated", stdout.second || stderr.second)
            .put("error", r.error)
    }

    /** The "error" code of an error body, or "" when the body is not one. */
    fun parseError(body: String): String = try {
        JSONObject(body).optString("error", "")
    } catch (e: Exception) {
        ""
    }

    /** Build.SUPPORTED_ABIS[0] -> protocol arch (amd64|arm64|386|arm). */
    fun archOf(abi: String): String = when (abi) {
        "arm64-v8a" -> "arm64"
        "x86_64" -> "amd64"
        "x86" -> "386"
        else -> "arm"
    }

    /** Android waits poll_interval_s clamped to 15..600 between syncs. */
    fun clampPoll(pollIntervalS: Int): Int = pollIntervalS.coerceIn(15, 600)

    private fun cap(s: String): Pair<String, Boolean> {
        val b = s.toByteArray(Charsets.UTF_8)
        if (b.size <= OUTPUT_CAP) return Pair(s, false)
        return Pair(String(b, 0, OUTPUT_CAP, Charsets.UTF_8), true)
    }
}
