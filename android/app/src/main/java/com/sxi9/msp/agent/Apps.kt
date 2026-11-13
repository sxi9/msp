package com.sxi9.msp.agent

import org.json.JSONArray
import org.json.JSONObject

// Pure Kotlin (no android.* imports): shapes the installed-apps inventory into the
// facts.apps array of docs/PROTOCOL.md (fixture sync_android_apps.json). The
// PackageManager read lives in AppInventory.kt; this file is exercised by AppsTest on
// the JVM.

/** One installed package as PackageManager reports it; label and version may be missing. */
data class InstalledApp(
    val packageName: String,
    val label: String?,
    val version: String?,
    val system: Boolean,
)

object Apps {
    /**
     * Caps. MAX_APPS matches the server's protocol.MaxApps. MAX_BYTES is the budget for the
     * serialised apps array: the agent body limit is 256 KiB and the rest of a sync body is
     * a few hundred bytes, so 192 KiB keeps the request under the limit even when every
     * field is at its maximum length (1000 x ~630 bytes would not be). Typical entries are
     * ~100 bytes, so the byte budget only bites on pathological inventories; the entry cap
     * is what normally applies. The field limits are the server's validation limits.
     */
    const val MAX_APPS = 1000
    const val MAX_BYTES = 192 * 1024
    const val MAX_PACKAGE = 256
    const val MAX_LABEL = 256
    const val MAX_VERSION = 64

    /**
     * The facts.apps array: non-system apps first, each group sorted by label
     * (case-insensitive, then by package for a stable order), a blank label replaced by
     * the package, a missing version sent as "", fields cut to the protocol limits, at
     * most MAX_APPS entries and MAX_BYTES of JSON (entries past either cap are dropped
     * from the end, so system apps go first).
     */
    fun toJson(apps: List<InstalledApp>): JSONArray {
        val sorted = apps
            .filter { it.packageName.isNotBlank() }
            .map { normalise(it) }
            .sortedWith(compareBy<InstalledApp> { it.system }.thenBy { it.label!!.lowercase() }.thenBy { it.packageName })
        val out = JSONArray()
        var bytes = 2 // the surrounding [ ]
        for (a in sorted) {
            if (out.length() >= MAX_APPS) break
            val o = JSONObject()
                .put("package", a.packageName)
                .put("label", a.label)
                .put("version", a.version)
                .put("system", a.system)
            val size = o.toString().toByteArray(Charsets.UTF_8).size + 1 // plus its comma
            if (bytes + size > MAX_BYTES) break
            bytes += size
            out.put(o)
        }
        return out
    }

    private fun normalise(a: InstalledApp): InstalledApp {
        val pkg = capBytes(a.packageName.trim(), MAX_PACKAGE)
        val label = a.label?.trim()?.takeIf { it.isNotEmpty() } ?: pkg
        return InstalledApp(
            packageName = pkg,
            label = capBytes(label, MAX_LABEL),
            version = capBytes(a.version?.trim() ?: "", MAX_VERSION),
            system = a.system,
        )
    }

    /** s cut to at most max UTF-8 bytes without splitting a code point (the server counts bytes). */
    fun capBytes(s: String, max: Int): String {
        if (s.toByteArray(Charsets.UTF_8).size <= max) return s
        val sb = StringBuilder()
        var used = 0
        var i = 0
        while (i < s.length) {
            val cp = s.codePointAt(i)
            val width = Character.charCount(cp)
            val size = String(Character.toChars(cp)).toByteArray(Charsets.UTF_8).size
            if (used + size > max) break
            sb.appendCodePoint(cp)
            used += size
            i += width
        }
        return sb.toString()
    }
}
