
Debian
====================
This directory contains files used to package whippetd/whippet-qt
for Debian-based Linux systems. If you compile whippetd/whippet-qt yourself, there are some useful files here.

## whippet: URI support ##


whippet-qt.desktop  (Gnome / Open Desktop)
To install:

	sudo desktop-file-install whippet-qt.desktop
	sudo update-desktop-database

If you build yourself, you will either need to modify the paths in
the .desktop file or copy or symlink your whippet-qt binary to `/usr/bin`
and the `../../share/pixmaps/whippet128.png` to `/usr/share/pixmaps`

whippet-qt.protocol (KDE)

