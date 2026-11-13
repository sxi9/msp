plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
}

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

    buildTypes {
        release {
            isMinifyEnabled = false
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
