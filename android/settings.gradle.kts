// The Argus app for Android. The agent itself is Go, bound into
// app/libs/argus.aar by gomobile (see ../.github/workflows/ci.yml, "Agent on
// Android"); this project is the Kotlin shell around it.
pluginManagement {
    repositories {
        google()
        mavenCentral()
        gradlePluginPortal()
    }
}
dependencyResolutionManagement {
    repositoriesMode.set(RepositoriesMode.FAIL_ON_PROJECT_REPOS)
    repositories {
        google()
        mavenCentral()
    }
}
rootProject.name = "argus-android"
include(":app")
include(":helper")
