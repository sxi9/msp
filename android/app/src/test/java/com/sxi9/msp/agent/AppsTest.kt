package com.sxi9.msp.agent

import org.json.JSONArray
import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test
import java.io.File

/** Apps.toJson: the facts.apps shape of docs/PROTOCOL.md, on the JVM without Android. */
class AppsTest {
    private val fixtures = File(
        System.getProperty("MSP_FIXTURES") ?: error("MSP_FIXTURES system property not set")
    )

    private fun app(pkg: String, label: String? = null, version: String? = "1.0", system: Boolean = false) =
        InstalledApp(pkg, label, version, system)

    private fun rows(arr: JSONArray): List<JSONObject> = (0 until arr.length()).map { arr.getJSONObject(it) }

    private fun keys(o: JSONObject): Set<String> = o.keys().asSequence().toSet()

    @Test
    fun entryKeysMatchFixture() {
        val fixture = JSONObject(fixtures.resolve("sync_android_apps.json").readText())
        val want = fixture.getJSONObject("facts").getJSONArray("apps").getJSONObject(0)
        val got = Apps.toJson(listOf(app("com.example", "Example", "1.2.3"))).getJSONObject(0)
        assertEquals(keys(want), keys(got))
        assertEquals("com.example", got.getString("package"))
        assertEquals("Example", got.getString("label"))
        assertEquals("1.2.3", got.getString("version"))
        assertFalse(got.getBoolean("system"))
    }

    @Test
    fun emptyInventoryIsAnEmptyArray() {
        // Nothing to report (or a failed PackageManager read, which AppInventory maps to an
        // empty list) goes on the wire as "apps":[]; the server stores that as no key
        // (docs/PROTOCOL.md 3.2a).
        val arr = Apps.toJson(emptyList())
        assertEquals(0, arr.length())
        val facts = JSONObject().put("apps", arr)
        assertEquals("{\"apps\":[]}", facts.toString())
        val body = Protocol.buildSyncJson(1, JSONObject(), facts)
        assertTrue(body.toString().contains("\"apps\":[]"))
        assertEquals(0, body.getJSONObject("facts").getJSONArray("apps").length())
    }

    @Test
    fun ordersNonSystemFirstThenByLabelCaseInsensitive() {
        val out = rows(
            Apps.toJson(
                listOf(
                    app("com.android.settings", "Settings", "14", system = true),
                    app("com.sxi9.msp.agent", "MSP Agent", "0.1.0"),
                    app("org.mozilla.firefox", "Firefox", "131.0.3"),
                    app("com.google.android.gms", "Google Play services", "24.38.33", system = true),
                    app("com.example.lower", "alpha tools", "1"),
                    app("com.example.b", "Same", "1"),
                    app("com.example.a", "Same", "1"),
                )
            )
        )
        assertEquals(
            listOf("com.example.lower", "org.mozilla.firefox", "com.sxi9.msp.agent", "com.example.a", "com.example.b", "com.google.android.gms", "com.android.settings"),
            out.map { it.getString("package") }
        )
        assertEquals(listOf(false, false, false, false, false, true, true), out.map { it.getBoolean("system") })
    }

    @Test
    fun missingVersionIsEmptyAndBlankLabelFallsBackToPackage() {
        val out = rows(Apps.toJson(listOf(app("com.example.nolabel", null, null), app("com.example.blank", "  ", ""))))
        assertEquals("com.example.blank", out[0].getString("label"))
        assertEquals("", out[0].getString("version"))
        assertEquals("com.example.nolabel", out[1].getString("label"))
        assertEquals("", out[1].getString("version"))
        // Blank packages cannot be reported (the server requires 1..256 characters).
        assertEquals(0, Apps.toJson(listOf(app("", "ghost"), app("   ", "ghost"))).length())
    }

    @Test
    fun capsAtMaxAppsDroppingSystemAppsFirst() {
        val many = (0 until 700).map { app("com.user.app%04d".format(it), "User $it") } +
            (0 until 700).map { app("com.sys.app%04d".format(it), "Sys $it", system = true) }
        val out = rows(Apps.toJson(many.shuffled()))
        assertEquals(Apps.MAX_APPS, out.size)
        assertEquals(700, out.count { !it.getBoolean("system") })
        assertEquals(300, out.count { it.getBoolean("system") })
        assertEquals("com.sys.app0000", out[700].getString("package"))
    }

    @Test
    fun fieldsAreCutToTheProtocolLimitsInBytes() {
        val out = rows(
            Apps.toJson(
                listOf(
                    app("p".repeat(Apps.MAX_PACKAGE + 5), "l".repeat(Apps.MAX_LABEL + 5), "v".repeat(Apps.MAX_VERSION + 5)),
                    app("com.utf8", "é".repeat(200), "ü".repeat(40)),
                )
            )
        )
        val long = out.first { it.getString("package").startsWith("ppp") }
        assertEquals(Apps.MAX_PACKAGE, long.getString("package").toByteArray(Charsets.UTF_8).size)
        assertEquals(Apps.MAX_LABEL, long.getString("label").toByteArray(Charsets.UTF_8).size)
        assertEquals(Apps.MAX_VERSION, long.getString("version").toByteArray(Charsets.UTF_8).size)
        val utf8 = out.first { it.getString("package") == "com.utf8" }
        // Two-byte characters: cut on a character boundary, never over the byte limit.
        assertEquals("é".repeat(128), utf8.getString("label"))
        assertEquals("ü".repeat(32), utf8.getString("version"))
        assertEquals("abc", Apps.capBytes("abc", 3))
        assertEquals("", Apps.capBytes("é", 1))
    }

    @Test
    fun byteBudgetKeepsTheSyncBodyUnderTheAgentLimit() {
        // Every field at its maximum: 1000 entries would be ~630 KiB, over the server's
        // 256 KiB agent body limit, so the byte budget must cut the list short.
        val worst = (0 until Apps.MAX_APPS).map {
            app("%04d".format(it) + "p".repeat(Apps.MAX_PACKAGE - 4), "l".repeat(Apps.MAX_LABEL), "v".repeat(Apps.MAX_VERSION))
        }
        val arr = Apps.toJson(worst)
        val bytes = arr.toString().toByteArray(Charsets.UTF_8).size
        assertTrue("apps array is $bytes bytes", bytes <= Apps.MAX_BYTES)
        assertTrue("only ${arr.length()} entries fit", arr.length() in 300..Apps.MAX_APPS - 1)
        val body = Protocol.buildSyncJson(1, JSONObject(), JSONObject().put("apps", arr))
        assertTrue(body.toString().toByteArray(Charsets.UTF_8).size < 256 * 1024)

        // Typical entries: all 1000 fit comfortably.
        val typical = (0 until Apps.MAX_APPS).map { app("com.vendor.product.component%04d".format(it), "Vendor Product $it", "123.456.789") }
        val arr2 = Apps.toJson(typical)
        assertEquals(Apps.MAX_APPS, arr2.length())
        assertTrue(arr2.toString().toByteArray(Charsets.UTF_8).size < Apps.MAX_BYTES)
    }
}
