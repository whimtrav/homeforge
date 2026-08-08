# Troubleshooting & hardware notes

## AMD Ryzen mini-PCs: random reboots or freezes at idle

**Symptom.** On some AMD Ryzen mini-PCs (Beelink, Minisforum, and similar), a general-purpose
Linux distro (Debian / Ubuntu / Proxmox) will spontaneously **hard-reset or freeze at idle** —
often at a fairly regular interval — with **no** kernel panic, OOM, MCE, or thermal warning in
the logs (a *silent* reset). A tell-tale sign is that it dies shortly after the machine goes
quiet, and stays stable while it's busy. Purpose-built appliance OSes (e.g. Home Assistant OS)
often do **not** hit this on the exact same hardware, because they ship a kernel/idle
configuration that sidesteps it — so it's not a hardware defect.

**Cause.** The SoC drops into a very-low-current package idle state (C6 / "low current idle")
that the board's power delivery can't sustain, and the machine resets. It lives **below the
OS**, which is why OS-level tweaks alone frequently don't fully fix it.

**Fix — in order of preference:**

1. **BIOS — the real cure.** Enter setup (usually `Del` / `Esc` / `F7`) and set:
   - **`Power Supply Idle Control → Typical Current Idle`** — typically under
     `Advanced → AMD CBS → CPU Common Options`, or `… → NBIO Common Options → SMU Common Options`.
     This is *the* fix on most boards.
   - If that option isn't exposed (some mini-PC BIOSes hide it), fall back to
     **`Global C-State Control → Disabled`** (`Advanced → AMD CBS → CPU Common Options`) and
     disable chipset power-saving / clock-gating — **`SB Power Saving`**, **`AB Clock Gating`**,
     **`PCIB Clock Run`** — under the **Chipset** menu. These keep the platform from deep-idling.
   - **Update the BIOS/AGESA** to the latest for your exact board first — newer firmware often
     fixes this outright and can expose the settings above.

2. **Kernel fallback (if you can't change BIOS).** Add to `GRUB_CMDLINE_LINUX_DEFAULT`
   (then `sudo update-grub` and reboot):
   ```
   processor.max_cstate=1 idle=nomwait
   ```
   Less reliable than the BIOS fix — it can't reach the firmware-level idle current — but worth
   trying. (Disabling C6 via ZenStates `zenstates.py --c6-disable` is another OS-side attempt.)

**Verify.** After the fix, `/sys/devices/system/cpu/cpu0/cpuidle/` should show few or no deep
idle states, and the machine should survive well past its usual reset interval when left idle.

---

## Headless / 24×7 server resilience

HomeForge is often run headless. A few settings let it self-heal instead of needing hands-on:

- **Hardware watchdog** — arm it so a *hang* auto-reboots instead of sitting dead. In
  `/etc/systemd/system.conf`: `RuntimeWatchdogSec=30`. (Make sure the watchdog module — e.g.
  `sp5100_tco` on AMD — exposes `/dev/watchdog`.)
- **Auto power-on** — set **`Restore on AC Power Loss → Power On`** (a.k.a. *State After G3*) in
  BIOS and **disable ErP / deep-standby**. Combined with a **smart plug driven from a *different*
  machine** (never the server itself), you can remotely power-cycle a truly-hung box.
- **Auto-start the stack** — the bundled `docker-compose.yml` uses `restart: unless-stopped`, so
  the hub comes back on its own after any reboot.
- **Rule out RAM** — if a box is unstable, run **MemTest86** (≥1 full pass) before chasing
  software causes.

> Note: consumer mini-PC BIOSes generally don't expose UEFI HII "firmware attributes" to Linux,
> so you can't reliably change BIOS settings from the OS (`fwupdmgr get-bios-settings` /
> `/sys/class/firmware-attributes` will be empty). For occasional remote BIOS access on a headless
> box, a small IP-KVM is the clean, brick-proof option.
