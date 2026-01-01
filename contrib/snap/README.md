# Whippet Snap Packaging

Commands for building and uploading a Whippet Core Snap to the Snap Store. Anyone on amd64 (x86_64), arm64 (aarch64), or i386 (i686) should be able to build it themselves with these instructions. This would pull the official Whippet binaries from the releases page, verify them, and install them on a user's machine.

## Building Locally
```
sudo apt install snapd
sudo snap install --classic snapcraft
sudo snapcraft
```

### Installing Locally
```
snap install \*.snap --devmode
```

### To Upload to the Snap Store
```
snapcraft login
snapcraft register whippet-core
snapcraft upload \*.snap
sudo snap install whippet-core
```

### Usage
```
whippet-unofficial.cli # for whippet-cli
whippet-unofficial.d # for whippetd
whippet-unofficial.qt # for whippet-qt
whippet-unofficial.test # for test_whippet
whippet-unofficial.tx # for whippet-tx
```

### Uninstalling
```
sudo snap remove whippet-unofficial
```