plugins {
    id("maven-publish")
}

val artifactFile = "ipfs.aar"
val sourcesFile = "ipfs-sources.jar"

configurations.maybeCreate("default")
artifacts.add("default", file(artifactFile))

publishing {
    publications {
        create<MavenPublication>("mavenAar") {
            artifactId = "ffi"

            artifact(file(artifactFile)) {
                extension = "aar"
            }

            artifact(file(sourcesFile)) {
                classifier = "sources"
                extension = "jar"
            }
        }
    }
    repositories {
        mavenLocal()
    }
}
