# layout-stack

Most-recently-used ("stack") keyboard layout switching for KDE Plasma on X11,
the way Gboard does it on Android.

Layouts are kept in a stack ordered by when you last *typed* in them. Caps Lock
walks down that stack, so the layout you need is almost always one or two
presses away, no matter how many layouts you have.

## How it works

Say you have `us`, `ru` and `epo`, and the stack is `[ru, us, epo]` (you're
typing in Russian).

- **Caps Lock** switches to the next layout in the stack, immediately: `ru → us`.
- **Caps Lock again** (without typing) goes deeper: `us → epo`, then wraps
  around back to `ru`.
- **Typing a character** moves the current layout to the top of the stack.

```
stack [ru, us, epo], current ru
Caps            → us                  stack unchanged
type "hello"    → stack [us, ru, epo]
Caps            → ru                  back where you were
Caps, Caps      → epo                 (ru → us → epo)
type "saluton"  → stack [epo, ru, us]
Caps            → ru
```

There is no timeout: switching never waits, and you can start typing right
after pressing Caps. A layout only "counts" once you have typed in it, so
cycling through layouts without typing doesn't reorder anything.

What counts as typing: any key that produces a character (letters, digits,
punctuation, space) pressed without Ctrl, Alt, Super or Hyper held. Shortcuts
like Ctrl+C, arrows, function keys, Enter, Backspace etc. don't count.

Layout changes made elsewhere (KDE shortcuts, the tray applet) are picked up
too: the stack knows where you are, and that layout moves to the top as soon
as you type in it.

## Requirements

- KDE Plasma (uses the `org.kde.keyboard` D-Bus service to switch layouts)
- X11 session with the `RECORD` extension (enabled in Xorg by default)
- Local display (`DISPLAY=:N`), authenticated via `MIT-MAGIC-COOKIE-1` or none
- Go to build (see `go.mod` for the version)

Wayland is not supported.

The result is a single statically linked binary with no runtime dependencies.

## Setup

### 1. Turn Caps Lock into a Menu key

layout-stack listens for the `Menu` keysym. Set the xkb option `caps:menu`
instead of any `grp:*` layout-switching option:

*System Settings → Keyboard → Key Bindings → Caps Lock behavior → Make Caps
Lock an additional Menu key*, and remove Caps Lock from "Switching to another
layout".

Or in `~/.config/kxkbrc`:

```ini
[Layout]
Options=caps:menu
```

(keep your other options, e.g. `caps:menu,compose:ralt`).

Note that this also makes Caps Lock no longer act as Caps Lock, and a physical
Menu key, if you have one, will switch layouts too.

### 2. Make sure no KDE shortcut uses Menu

layout-stack grabs the Menu key globally; the grab fails if another program,
typically KDE's global shortcuts, already holds it. Check *System Settings →
Shortcuts* (e.g. "Switch to Last-Used Keyboard Layout" under Keyboard Layout
Switcher).

### 3. Build and install

```sh
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o ~/.local/bin/layout-stack .
```

### 4. Run it as a systemd user service

```sh
cp layout-stack.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now layout-stack.service
```

The service starts with the graphical session. It needs `DISPLAY` and
`XAUTHORITY` in the systemd user environment; Plasma exports them. Check with
`systemctl --user show-environment`.

Logs:

```sh
journalctl --user -u layout-stack -f
```

On start and whenever the order changes it logs the stack, e.g.
`stack: ru us epo`.

### Updating

```sh
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o ~/.local/bin/layout-stack . \
  && systemctl --user restart layout-stack
```

### Uninstalling

```sh
systemctl --user disable --now layout-stack
rm ~/.local/bin/layout-stack ~/.config/systemd/user/layout-stack.service
```

Then restore your previous Caps Lock / layout switching options.

## Implementation notes

Everything is in `main.go`. The daemon has three inputs, merged in one event
loop so that their order is preserved:

- **Key events** come from the X `RECORD` extension, which delivers device-level
  KeyPress/KeyRelease events for all clients. Both typing detection and Caps
  presses are taken from this one stream, so a fast "Caps, type, Caps" sequence
  is never reordered. xgb can't consume the stream of replies that
  `RecordEnableContext` produces, so this connection is a small hand-written X
  client (connection setup, Xauthority lookup, three RECORD requests).
- **The Menu key grab** (`XGrabKey` via xgb) only stops Menu from reaching
  applications. The keymap is reloaded and the key re-grabbed on
  `MappingNotify`, which KDE triggers when it re-applies the keyboard
  configuration.
- **KDE D-Bus** (`org.kde.keyboard /Layouts`): `setLayout` to switch,
  `layoutChanged` to notice switches made elsewhere, `layoutListChanged` to
  pick up configuration changes.

Two KDE quirks are handled:

- KDE emits `layoutChanged` twice for each switch. Signals for a layout that
  layout-stack itself selected less than a second ago are ignored, so they
  aren't mistaken for an external switch during fast repeated presses.
- KDE sometimes emits `layoutListChanged` when nothing changed. The stack
  is kept unless the list of layouts actually differs.

Character keys are detected by the keysym on the first group/level: anything
except `0xfe00–0xffff` (function, modifier, keypad, editing keys) and vendor
keysyms like `XF86*`.

### Privacy

layout-stack sees every key press, like any program using `RECORD`. It only
checks whether the key produces a character and whether a modifier is held;
it doesn't store key presses or write them anywhere. Only layout names
appear in the log.

## Limitations

- X11 only. On Wayland, global key monitoring like this isn't available to
  ordinary clients.
- KDE only, because switching goes through KDE's D-Bus API.
- Key autorepeat on Caps Lock (if enabled for that key) counts as repeated
  presses.
- Remote / TCP displays are not supported by the RECORD connection.
