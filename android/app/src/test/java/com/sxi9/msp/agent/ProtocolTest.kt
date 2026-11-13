package com.sxi9.msp.agent

import org.json.JSONArray
import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test
import java.io.File

/**
 * Contract test against the canonical fixtures in internal/protocol/fixtures, which the Go
 * side round-trips too: a shape change breaks both suites until both agents are updated.
 */
class ProtocolTest {
    private val fixtures = File(
        System.getProperty("MSP_FIXTURES") ?: error("MSP_FIXTURES system property not set")
    )

    private fun fixture(name: String): String = fixtures.resolve(name).readText()

    private fun keys(o: JSONObject): Set<String> = o.keys().asSequence().toSet()

    private fun sampleMetrics(): JSONObject = JSONObject()
        .put("cpu_pct", JSONObject.NULL)
        .put("mem_total_mb", 7634)
        .put("mem_used_mb", 4210)
        .put("disks", JSONArray().put(JSONObject().put("mount", "/data").put("total_gb", 107.2).put("used_gb", 61.8)))
        .put("battery_pct", 87)
        .put("battery_charging", false)
        .put("ips", JSONArray().put("192.168.1.23"))

    private fun sampleFacts(): JSONObject = JSONObject()
        .put("kernel", "5.15.110-android14-11")
        .put("cpu_model", "shiba")
        .put("cpu_count", 9)
        .put("mem_total_mb", 7634)
        .put("manufacturer", "Google")
        .put("model", "Pixel 8")
        .put("serial", "")
        .put("ips", JSONArray().put("192.168.1.23"))

    @Test
    fun enrollMatchesFixture() {
        val want = JSONObject(fixture("enroll_android.json"))
        val got = Protocol.buildEnrollJson(
            "et_Ab12cD3eFgHiJkLmNoPqRsTuVwXyZ0123456789abcd", "9774d56d682e549c",
            "Pixel 8", "arm64", "Android 14 (34)", sampleFacts()
        )
        assertEquals(keys(want), keys(got))
        assertEquals(keys(want.getJSONObject("facts")), keys(got.getJSONObject("facts")))
        assertEquals("android", got.getString("platform"))
        assertEquals("0.1.0", got.getString("agent_version"))
        assertEquals(60, got.getInt("interval_s"))
        assertEquals("", got.getJSONObject("facts").getString("serial"))
    }

    @Test
    fun syncMatchesFixture() {
        val want = JSONObject(fixture("sync_android.json"))
        val got = Protocol.buildSyncJson(345600, sampleMetrics(), null)
        assertEquals(keys(want), keys(got))
        assertEquals(keys(want.getJSONObject("metrics")), keys(got.getJSONObject("metrics")))
        val m = got.getJSONObject("metrics")
        assertEquals(JSONObject.NULL, m.get("cpu_pct"))
        assertTrue(m.get("battery_charging") is Boolean)
        assertEquals("/data", m.getJSONArray("disks").getJSONObject(0).getString("mount"))
        assertEquals(0, got.getInt("wait_s"))
        assertEquals(60, got.getInt("interval_s"))

        val withFacts = Protocol.buildSyncJson(1, sampleMetrics(), sampleFacts())
        assertEquals(keys(want) + "facts", keys(withFacts))
        assertEquals(keys(JSONObject(fixture("sync_linux.json")).getJSONObject("facts")), keys(withFacts.getJSONObject("facts")))
    }

    @Test
    fun syncWithAppsMatchesFixture() {
        // The installed-apps inventory rides inside facts as "apps" (sync_android_apps.json).
        val want = JSONObject(fixture("sync_android_apps.json"))
        val apps = Apps.toJson(
            listOf(
                InstalledApp("com.android.settings", "Settings", "14", true),
                InstalledApp("com.sxi9.msp.agent", "MSP Agent", "0.1.0", false),
                InstalledApp("com.example.nolabel", null, null, false),
                InstalledApp("org.mozilla.firefox", "Firefox", "131.0.3", false),
                InstalledApp("com.google.android.gms", "Google Play services", "24.38.33", true),
            )
        )
        val got = Protocol.buildSyncJson(345600, sampleMetrics(), sampleFacts().put("apps", apps))
        assertEquals(keys(want), keys(got))
        assertEquals(keys(want.getJSONObject("facts")), keys(got.getJSONObject("facts")))
        val wantApps = want.getJSONObject("facts").getJSONArray("apps")
        val gotApps = got.getJSONObject("facts").getJSONArray("apps")
        assertEquals(wantApps.length(), gotApps.length())
        for (i in 0 until wantApps.length()) {
            assertEquals(wantApps.getJSONObject(i).toString(), gotApps.getJSONObject(i).toString())
        }
    }

    @Test
    fun parsesSyncResponse() {
        assertEquals(30, Protocol.parseSyncResponse(fixture("sync_response.json")))
        // exec/script are desktop kinds; parsing keeps them so the service can reject them.
        val desktop = Protocol.parseSyncCommands(fixture("sync_response.json"))
        assertEquals(listOf("exec", "script"), desktop.map { it.kind })

        val android = Protocol.parseSync(fixture("sync_response_android.json"))
        assertEquals(60, android.pollIntervalS)
        assertEquals(
            listOf(Command(9, "lock", "", 30), Command(10, "ping", "", 30), Command(11, "message", "Please call the helpdesk", 30)),
            android.commands
        )
        assertTrue(Protocol.parseSyncCommands("""{"poll_interval_s":60,"commands":[]}""").isEmpty())
    }

    @Test
    fun parsesEnrollResponse() {
        val r = Protocol.parseEnrollResponse("""{"device_id":42,"device_token":"dt_x","poll_interval_s":60}""")
        assertEquals(42L, r.deviceId)
        assertEquals("dt_x", r.deviceToken)
        assertEquals(60, r.pollIntervalS)
    }

    @Test
    fun parsesError() {
        assertEquals("invalid_device_token", Protocol.parseError(fixture("error.json")))
        assertEquals("", Protocol.parseError("not json"))
    }

    @Test
    fun resultMatchesFixture() {
        val want = JSONObject(fixture("command_result.json"))
        val got = Protocol.buildResultJson(
            CommandResult(0, "pong", "", "2026-11-13T10:00:01Z", "2026-11-13T10:00:01Z", "")
        )
        assertEquals(keys(want), keys(got))
        assertEquals(false, got.getBoolean("truncated"))
        assertEquals("pong", got.getString("stdout"))

        val big = Protocol.buildResultJson(
            CommandResult(0, "x".repeat(OUTPUT_CAP + 10), "", "2026-11-13T10:00:01Z", "2026-11-13T10:00:01Z", "")
        )
        assertEquals(true, big.getBoolean("truncated"))
        assertEquals(OUTPUT_CAP, big.getString("stdout").length)
    }

    @Test
    fun archAndPollClamp() {
        assertEquals("arm64", Protocol.archOf("arm64-v8a"))
        assertEquals("amd64", Protocol.archOf("x86_64"))
        assertEquals("386", Protocol.archOf("x86"))
        assertEquals("arm", Protocol.archOf("armeabi-v7a"))
        assertEquals(15, Protocol.clampPoll(5))
        assertEquals(60, Protocol.clampPoll(60))
        assertEquals(600, Protocol.clampPoll(3600))
        assertEquals("msp-agent/0.1.0 (android/arm64)", ApiClient.userAgent("arm64"))
    }
}
