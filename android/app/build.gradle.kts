plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
}

// The app's version is the control plane's: the release workflow passes it
// in, so a phone's app and its Argus are told apart by the same number.
val argusVersion: String = (project.findProperty("argusVersion") as String?) ?: "0.0.0-dev"
val argusVersionCode: Int = (project.findProperty("argusVersionCode") as String?)?.toIntOrNull() ?: 1

android {
    namespace = "com.gsoultan.argus"
    compileSdk = 34

    defaultConfig {
        applicationId = "com.gsoultan.argus"
        // 29: a foreground service may capture the screen; 28 gives the
        // accessibility service Lock screen, 24 gestures.
        minSdk = 29
        targetSdk = 34
        versionCode = argusVersionCode
        versionName = argusVersion
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
    kotlinOptions {
        jvmTarget = "17"
    }
    sourceSets["main"].java.srcDirs("src/main/kotlin")
    packaging {
        resources.excludes += "META-INF/*.md"
    }
}

dependencies {
    // The agent: Go, bound by gomobile. Built by CI, or by hand with
    //   gomobile bind -target android -androidapi 29 -o android/app/libs/argus.aar ./mobile
    implementation(files("libs/argus.aar"))

    // The unattended helper, for its wire protocol (Protocol): the app talks
    // to the helper over a local socket in this framing. The helper's own
    // dex is pushed to the device separately (assets/argus-helper.dex).
    implementation(project(":helper"))

    implementation("androidx.core:core-ktx:1.13.1")
    implementation("androidx.appcompat:appcompat:1.7.0")
    implementation("com.google.android.material:material:1.12.0")
    // The console's code, scanned with the camera. Maven Central only.
    implementation("com.journeyapps:zxing-android-embedded:4.3.0")
    implementation("com.google.zxing:core:3.5.3")
}
