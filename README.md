# ScanSpool

ScanSpool is an independent tool compatible with the **ScanSnap iX1300**. It receives scans and saves them as PDFs in a **local folder** (including a Paperless consume folder) or on an **FTP server**. Use its terminal interface to set up the scanner and change its profile.

[First scan](#2-make-your-first-scan) · [Run in the background](#3-keep-it-running) · [Update](#update-the-service) · [Troubleshooting](#troubleshooting)

## 1. Download

Open [GitHub Releases][releases], select the latest release and expand **Assets**. Download the executable for the machine that will receive scans. The **Source code** archives are for developers.

| Receiver | x86-64 (Intel or AMD) | ARM64 |
| --- | --- | --- |
| Linux or Synology | `scanspool-linux-amd64` | `scanspool-linux-arm64` |
| macOS | `scanspool-darwin-amd64` | `scanspool-darwin-arm64` (Apple silicon) |
| Windows | `scanspool-windows-amd64.exe` | `scanspool-windows-arm64.exe` |

On Linux or Synology, `uname -m` reports `x86_64` or `aarch64` for the two architectures above.

<details>
<summary>Copy the download to a Synology</summary>

Enable SSH on the NAS. On the computer where you downloaded the file, open a terminal in the download folder. Replace `synology` with your SSH alias or `user@nas-address`. Use the filename that matches the NAS.

```sh
scp scanspool-linux-amd64 synology:scanspool-linux-amd64
ssh synology
```

If the copy says `subsystem request failed`, retry with `scp -O scanspool-linux-amd64 synology:scanspool-linux-amd64`.

</details>

## 2. Make your first scan

The commands below assume the x86-64 Linux download is in your home directory on the receiver. Substitute the filename you downloaded. On macOS use the same setup commands with the macOS file. On Windows, open PowerShell in the download folder and run the chosen executable with `setup`, for example `.\scanspool-windows-amd64.exe setup`.

On Linux or macOS, open setup as your normal user.

```sh
chmod +x ~/scanspool-linux-amd64
~/scanspool-linux-amd64 setup
```

1. **Connect the scanner.** Close ScanSnap Home or other receivers first. In **Set up / change scanner**, enter the scanner's IPv4 address or leave it blank to discover it on the local network. Select the scanner and enter its connection password if requested. Confirm the new computer registration to make this machine the scan receiver.
2. **Choose where PDFs go.** In **Output destination**, choose **Local folder** and enter an absolute path on the receiver (for example, `/volume1/scans` on a NAS). For **FTP**, enter the host, login and an existing absolute server directory such as `/consume`.
3. **Test it.** Load a page and press the scanner button or select **Start scan** in the terminal interface. Check **Scanner status** for progress and look for the PDF in your chosen destination.

## 3. Keep it running

**Linux with systemd.** After the test scan finishes, exit setup. Install the service from the same account you used for setup. The system commands use `sudo`. The service itself runs as your account.

```sh
sudo ~/scanspool-linux-amd64 service install \
  --user "$(id -un)" \
  --data-dir "${XDG_CONFIG_HOME:-$HOME/.config}/scanspool"
sudo /usr/local/bin/scanspool service start
/usr/local/bin/scanspool setup
```

If you used `--data-dir` or `SCANSPOOL_DATA_DIR` during setup, pass the existing absolute data path to the installer. Installation enables autostart. After `service start`, the receiver keeps running when you close setup or disconnect SSH.

**Foreground mode.** Exit setup and run `~/scanspool-linux-amd64 run` (or `run` with the executable for your platform). Keep that terminal open while receiving scans. Open `setup` in another terminal for settings and live status. Ctrl+C stops the foreground receiver.

## Update the service

Download the newer executable from [GitHub Releases][releases] and put it in your home directory on the receiver. The download can keep the same filename. The installer reuses your existing account, scanner registration, settings and queued PDFs. Wait for any active scan to finish before restarting.

```sh
chmod +x ~/scanspool-linux-amd64
sudo ~/scanspool-linux-amd64 service install
sudo /usr/local/bin/scanspool service restart
/usr/local/bin/scanspool setup
```

The TUI header shows its version. If the running receiver reports a different version, the TUI shows it on a separate line. Use `/usr/local/bin/scanspool --version` to check the installed executable from the shell.

<details>
<summary>Switch from ScanSnap2FTP to ScanSpool</summary>

ScanSpool uses the same data files. Reuse the original data directory to keep the scanner registration, scan profile, output settings and queued PDFs.

1. Run `/usr/local/bin/scansnap2ftp service status` and note the **User**, **Data directory** and **Autostart** values. Download the new executable before stopping the old receiver.
2. Wait for the current scan to finish. Stop the old receiver and disable its autostart. Keep its files in place until you have tested ScanSpool. Back up the original data directory privately after stopping the service.

```sh
sudo /usr/local/bin/scansnap2ftp service stop
sudo systemctl disable scansnap2ftp.service
```

3. Install ScanSpool with the user and exact absolute data path you noted above. Replace the example values before running the commands.

```sh
chmod +x ~/scanspool-linux-amd64
sudo ~/scanspool-linux-amd64 service install \
  --user 'YOUR_RECEIVER_USER' \
  --data-dir '/absolute/path/to/original/data'
sudo /usr/local/bin/scanspool service start
/usr/local/bin/scanspool status
/usr/local/bin/scanspool setup
```

4. Check the scan profile and output folder in setup, then make a test scan. Once the PDF arrives, remove the old service with `sudo /usr/local/bin/scansnap2ftp service uninstall`. The shared data directory is retained.

If the switch fails before removing the old service, remove the new one with `sudo /usr/local/bin/scanspool service uninstall` when installed. Then start the old service with `sudo /usr/local/bin/scansnap2ftp service start`. If the old service had autostart enabled, restore it with `sudo systemctl enable scansnap2ftp.service`. Use one receiver at a time.

For a foreground installation, stop the old receiver and run ScanSpool with `--data-dir /absolute/path/to/original/data`. Use that same path for setup and the other commands, or set `SCANSPOOL_DATA_DIR`.

</details>

## Change scan settings

In the TUI, open **Scan profile**. Use ↑/↓ to move and ←/→ to change a choice. Ctrl+U clears a number field so you can type a new value. **Enter** saves, **Esc** discards, and **Ctrl+R** loads defaults into the draft. The default is color, 300 dpi, duplex and automatic paper size. Changes apply to the next scan, including scans started at the scanner.

<details>
<summary>CLI examples and profile options</summary>

These commands use the installed service executable. In foreground mode, use the path to your downloaded executable instead. Run them as your normal receiver account.

```sh
/usr/local/bin/scanspool profile show
/usr/local/bin/scanspool profile set --color gray --dpi 200 --sides simplex
/usr/local/bin/scanspool profile set --help
/usr/local/bin/scanspool profile reset
```

`profile set` changes only the supplied settings. All three commands accept `--data-dir` and `--json`. The CLI can edit the saved profile when the receiver is stopped. Older receivers need an update and restart before they can accept profile changes.

You can set color mode, resolution, sides, paper size, custom width and height, JPEG compression, black and white density, multi-feed detection, bleed-through reduction and blank-page removal. Auto color requires auto resolution. Nonzero density requires black and white mode. Custom dimensions use 1/1200 inch and 0 uses the paper preset. JPEG compression runs from 1 (best quality) to 5 (smallest file).

Custom scanner setting combinations are still awaiting physical validation. If a custom combination fails, try the default profile. Blank-page removal filters uniformly near-white pages after the entire batch arrives. A batch with no pages left produces no PDF.

</details>

## Troubleshooting

<details>
<summary>The scanner does not appear, or registration was interrupted</summary>

Switch it on and wait for its Wi-Fi connection. Enter its IPv4 address directly if discovery fails, particularly across subnets. If the TUI reports an interrupted registration, use **Recover previous registration**. Keep the recovery state and reuse the same data directory. Run one receiver per scanner.

</details>

<details>
<summary>A PDF is queued, or the output folder is inaccessible</summary>

Open **Scanner status** for the last error. For local output, the receiver account needs write access and permission to traverse parent folders. The app checks folder access when saving settings and before scanning. On Linux it shows a manual permission repair hint when available. After fixing access, choose **Retry queued deliveries**.

Completed PDFs stay in the private `outbox/` until delivery succeeds. The running receiver retries every 15 seconds. Each job stays linked to its original destination. Restore that destination to resend old jobs or recover the PDFs from the outbox. A canceled or incomplete scan does not publish a partial PDF.

FTP requires an existing absolute directory and permission to upload and rename files. A blank password field keeps the saved password. The empty-password toggle clears it.

</details>

<details>
<summary>Check logs, control the service, or remove it</summary>

```sh
/usr/local/bin/scanspool service status  # service process and autostart
/usr/local/bin/scanspool status          # scanner and queue
/usr/local/bin/scanspool status --json
/usr/local/bin/scanspool scan            # start a scan on the running receiver
/usr/local/bin/scanspool retry           # retry delivery
sudo /usr/local/bin/scanspool service logs --follow
sudo /usr/local/bin/scanspool service stop
sudo /usr/local/bin/scanspool service start
sudo /usr/local/bin/scanspool service restart
sudo /usr/local/bin/scanspool service uninstall
```

The service commands require Linux with systemd (including systemd 219). Start/stop/restart/uninstall need administrator rights. Journal access depends on host permissions. Foreground `run` logs to stderr and standalone setup writes `scanspool.log` inside its data directory.

`stop` preserves autostart. Stop or restart cancels an active scan. Uninstall removes the managed service files and leaves the registration, settings, outbox and output PDFs in place. A DSM upgrade may require reinstalling the service.

</details>

<details>
<summary>Data directory, compatibility and network details</summary>

Data path precedence is `--data-dir`, then `SCANSPOOL_DATA_DIR`, then the installed Linux service path for the receiver account, then a `scanspool` directory in the operating system's user config directory. Back it up privately. It holds the scanner registration, FTP credentials, scan profile and queued PDFs. Internal Unix directories and files use modes 0700 and 0600 respectively.

The service installs to `/usr/local/bin/scanspool` and stores the account and data path in `/etc/scanspool/service.json`. To change that binding, uninstall and reinstall using the intended existing directory. For old systemd compatibility, the home and data paths must avoid percent signs, double quotes, backslashes, control characters and trailing whitespace.

- Scanner support is **iX1300, interface version 16**. The receiver and scanner must reach each other over IPv4.
- Network ports include UDP 52217 for discovery, UDP 53220 for advertisements, UDP 55265 for button notifications and TCP 53218/53219 for scanner sessions. Only one receiver can bind the button port on a host.
- FTP uses plain FTP with passive data connections. Local delivery requires a filesystem with hard links. Files appear under their final names only after publication. A crash between publication and queue cleanup can cause redelivery.
- Local output and `outbox/` are separate. Local files use the receiver's umask. Scanner JPEGs are embedded as received and TIFF pages are converted losslessly. OCR can run downstream.
- TUI administration uses an authenticated loopback connection. Windows builds are available for testing. The documented system service is for Linux.

</details>

## Build from source

<details>
<summary>Developer commands and release files</summary>

Requires Go 1.26 or newer for development. The downloaded executable has no Go, Docker, Python or ScanSnap Home runtime dependency.

```sh
make build
make test
make cross-build
python3 tools/test_terminal.py ./bin/scanspool
```

`make build` writes `bin/scanspool`. `make cross-build` writes the six executables listed above. Builds use `CGO_ENABLED=0`. For a GitHub release, attach those files alongside [LICENSE](LICENSE) and [NOTICE](NOTICE). Override the Makefile version with `make build VERSION=0.2.0-test`. A direct `go build` reports `dev` unless the version is supplied.

`tools/test_systemd.py` provides a disposable Linux container check with a simulated service manager. Use the target machine to verify real boot and restart behavior.

</details>

The scanner protocol comes from [AirScap](https://github.com/mzyy94/airscap). See [LICENSE](LICENSE) and [NOTICE](NOTICE).

[releases]: ../../releases
