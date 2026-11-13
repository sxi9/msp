plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
}

// Release signing comes from the environment so no key material is ever in the tree:
//   MSP_ANDROID_KEYSTORE          path to a PKCS12/JKS keystore (keytool -genkeypair ...)
//   MSP_ANDROID_KEYSTORE_PASSWORD its store password
//   MSP_ANDROID_KEY_ALIAS         the key alias (default "msp-agent")
//   MSP_ANDROID_KEY_PASSWORD      the key password (default: the store password)
// Without MSP_ANDROID_KEYSTORE assembleRelease still works but produces
// app-release-unsigned.apk, which scripts/build-agents.sh refuses to publish.
val releaseKeystore: String? = System.getenv("MSP_ANDROID_KEYSTORE")?.takeIf { it.isNotBlank() }

android {
    namespace = "com.sxi9.msp.agent"
    compileSdk = 35
    buildToolsVersion = "35.0.0"

    defaultConfig {
        applicationId = "com.sxi9.msp.agent"
        minSdk = 26
        targetSdk = 35
        versionCode = 1
        versionName = rootDir.resolve("../VERSION").readText().trim()
    }

    buildFeatures {
        // BuildConfig.DEBUG gates the http:// (cleartext) allowance in MainActivity.
        buildConfig = true
    }

    signingConfigs {
        if (releaseKeystore != null) {
            create("release") {
                storeFile = file(releaseKeystore)
                storePassword = System.getenv("MSP_ANDROID_KEYSTORE_PASSWORD") ?: ""
                keyAlias = System.getenv("MSP_ANDROID_KEY_ALIAS")?.takeIf { it.isNotBlank() } ?: "msp-agent"
                keyPassword = System.getenv("MSP_ANDROID_KEY_PASSWORD")?.takeIf { it.isNotBlank() }
                    ?: (System.getenv("MSP_ANDROID_KEYSTORE_PASSWORD") ?: "")
            }
        }
    }

    buildTypes {
        release {
            // The release build is what gets published: not debuggable, no cleartext
            // (main/res/xml/network_security_config.xml), signed with the MSP's own key.
            isMinifyEnabled = false
            isDebuggable = false
            if (releaseKeystore != null) {
                signingConfig = signingConfigs.getByName("release")
            }
        }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }

    testOptions {
        unitTests.isReturnDefaultValues = true
        unitTests.all {
            // ProtocolTest reads the canonical JSON samples shared with the Go side.
            it.systemProperty("MSP_FIXTURES", rootDir.resolve("../internal/protocol/fixtures").absolutePath)
        }
    }
}

kotlin {
    compilerOptions {
        jvmTarget.set(org.jetbrains.kotlin.gradle.dsl.JvmTarget.JVM_17)
    }
}

dependencies {
    // No runtime dependencies: the app uses only the Android platform and the Kotlin stdlib.
    testImplementation("junit:junit:4.13.2")
    testImplementation("org.json:json:20240303")
}
