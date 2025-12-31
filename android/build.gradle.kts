// Top-level build file where you can add configuration options common to all sub-projects/modules.
plugins {
    alias(libs.plugins.android.library) apply false
    alias(libs.plugins.jetbrains.kotlin.android) apply false
}

subprojects {
    group = "com.acurast.ipfs"
    version = "1.1.1-beta01"
}