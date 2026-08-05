# CI toolchain: Go for the test suite, plus the Android SDK, NDK and a JDK,
# because the gomobile bind links code the Go tests never touch and has to be
# verified separately.
FROM golang:1.25-bookworm

### Prepare environment ###

RUN apt-get update -qq && \
    apt-get install -y -qq \
        wget \
        unzip \
        openjdk-17-jdk-headless \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

### Install Android SDK ###

# Keep in step with jitpack.yml and android/app/build.gradle.kts.
ENV NDK_VERSION=29.0.14206865
ENV ANDROID_HOME=/usr/lib/android-sdk
ENV ANDROID_COMPILE_SDK=34
ENV ANDROID_BUILD_TOOLS=34.0.0
ENV ANDROID_CMDLINE_TOOLS=13114758

RUN wget -q https://dl.google.com/android/repository/commandlinetools-linux-${ANDROID_CMDLINE_TOOLS}_latest.zip -O android-commandlinetools.zip && \
    unzip -q android-commandlinetools.zip -d ${ANDROID_HOME} && \
    mv ${ANDROID_HOME}/cmdline-tools ${ANDROID_HOME}/latest && \
    mkdir ${ANDROID_HOME}/cmdline-tools && \
    mv ${ANDROID_HOME}/latest ${ANDROID_HOME}/cmdline-tools && \
    rm android-commandlinetools.zip

ENV PATH="$PATH:${ANDROID_HOME}/cmdline-tools/latest/bin:${ANDROID_HOME}/platform-tools"

RUN yes | sdkmanager --sdk_root="${ANDROID_HOME}" --licenses

# Explicit rather than left to Gradle's on-demand download, so a build cannot
# fail on a Google endpoint.
RUN sdkmanager --sdk_root="${ANDROID_HOME}" \
        "platform-tools" \
        "platforms;android-${ANDROID_COMPILE_SDK}" \
        "build-tools;${ANDROID_BUILD_TOOLS}" \
        "ndk;${NDK_VERSION}"

# build_ffi requires this to locate the bind toolchain.
ENV ANDROID_NDK_HOME=${ANDROID_HOME}/ndk/${NDK_VERSION}

### Install Go tooling ###

# go-junit-report turns `go test` output into the JUnit XML GitLab renders.
RUN go install golang.org/x/mobile/cmd/gomobile@latest && \
    go install github.com/jstemmer/go-junit-report/v2@latest && \
    go install golang.org/x/vuln/cmd/govulncheck@latest && \
    gomobile init

### Prepare working directory ###

WORKDIR /app

# Dependencies before sources, so editing a .go file keeps the download layer.
COPY go.mod go.sum ./
RUN go mod download

# Same for the Gradle distribution, which the wrapper fetches on first use.
COPY android/gradle ./android/gradle
COPY android/gradlew ./android/gradlew
RUN chmod +x android/gradlew && (cd android && ./gradlew --version > /dev/null)

COPY . .

RUN chmod +x build_ffi android/gradlew
