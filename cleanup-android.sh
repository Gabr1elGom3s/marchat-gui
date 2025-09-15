#!/bin/bash
# cleanup-android.sh - Undo Android build artifacts and NDK installation
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ANDROID_NDK_VERSION="28.2.13676358"
APP_NAME="marchat-gui"

RED='\033[0;31m'
GREEN='\033[0;32m'
NC='\033[0m'

print_status() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

print_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

# Remove generated APKs
cleanup_apks() {
    print_status "Removing generated APKs..."
    cd "$SCRIPT_DIR"
    shopt -s nullglob
    APKS=(*.apk)
    if [ ${#APKS[@]} -gt 0 ]; then
        rm -f *.apk
        print_status "Removed ${#APKS[@]} APK(s)"
    else
        print_status "No APK files to remove"
    fi
}

# Remove installed NDK
cleanup_ndk() {
    if [ -z "$ANDROID_HOME" ]; then
        if [ -d "$HOME/Android/Sdk" ]; then
            export ANDROID_HOME="$HOME/Android/Sdk"
        else
            print_status "ANDROID_HOME not set and SDK not found; skipping NDK cleanup"
            return
        fi
    fi

    NDK_PATH="$ANDROID_HOME/ndk/$ANDROID_NDK_VERSION"
    if [ -d "$NDK_PATH" ]; then
        print_status "Removing NDK $ANDROID_NDK_VERSION..."
        rm -rf "$NDK_PATH"
        print_status "NDK removed"
    else
        print_status "NDK $ANDROID_NDK_VERSION not found; nothing to remove"
    fi
}

### MAIN ###
print_status "Starting cleanup..."
cleanup_apks
cleanup_ndk
print_status "Cleanup complete!"
