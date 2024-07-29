#!/bin/bash

CURR_DIR=${BASH_SOURCE[0]%/build_ffi.sh}

function check_tools () {
    echo "Checking the environment..."

    echo "  [tools]"
    if which go >/dev/null; then
        echo -e "    \xE2\x9C\x94 go"
    else
        echo -e "    \xE2\x9C\x97 go"
        ERROR="go has not been found"
    fi
    if which gomobile >/dev/null; then
        echo -e "    \xE2\x9C\x94 gomobile"
    else
        echo -e "    \xE2\x9C\x97 gomobile"
        ERROR="gomobile has not been found"
    fi

    echo "  [variables]"
    if [[ -z "${ANDROID_HOME}" ]]; then
        echo -e "    \xE2\x9C\x97 ANDROID_HOME"
        ERROR="ANDROID_HOME has not been set"
    else
        echo -e "    \xE2\x9C\x94 ANDROID_HOME"
    fi

    if [[ -z "${ANDROID_NDK_HOME}" ]]; then
        echo -e "    \xE2\x9C\x97 ANDROID_NDK_HOME"
        ERROR="ANDROID_NDK_HOME has not been set"
    else
        echo -e "    \xE2\x9C\x94 ANDROID_NDK_HOME"
    fi

    if [[ -n "${ERROR}" ]]; then
        echo "Error: $ERROR."
        exit 1
    fi
}

function init () {
    echo "Initializing gomobile tools..."
    gomobile init
}

function bind () {
    echo "Binding with gomobile..."

    go get golang.org/x/mobile
    
    ANDROID_API=$(grep "minSdk" $CURR_DIR/android/app/build.gradle.kts | awk '{print $3}' | tr -d \''"\')
    echo "  [android]"
    echo -en "    / API $ANDROID_API"
    if gomobile bind -o $CURR_DIR/android/ffi/ipfs.aar -target=android/arm,android/arm64 -androidapi $ANDROID_API ./ffi; then
        echo -e "\r    \xE2\x9C\x94 API $ANDROID_API"
    else
        echo -e "\r    \xE2\x9C\x97 API $ANDROID_API"
    fi

    go mod tidy
}

check_tools
init
bind

echo -e "\nDone."