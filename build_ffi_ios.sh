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
}

function init () {
    echo "Initializing gomobile tools..."
    gomobile init
}

function bind () {
    echo "Binding with gomobile..."
    
    go get golang.org/x/mobile

    gomobile bind -target=ios $CURR_DIR/ffi

    go mod tidy
}

check_tools
init
bind

echo -e "\nDone."