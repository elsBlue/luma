# Luma

Mirror and control an Android phone from Windows. No account. No paid tier.

Luma shows the phone on your PC and lets you use the mouse, keyboard, screenshots, recording, APK install, and file copy.

## Download

Open the latest release and download **Luma-Windows.zip** (ready to run) or **Luma.exe** (downloads its helper tools the first time you open it).

https://github.com/elsBlue/luma/releases/latest

Unzip `Luma-Windows.zip`, then open `Luma.exe`.

Windows may say the app is unrecognized because it is not code-signed. Choose **More info**, then **Run anyway**.

## First connection

1. On the phone, open **Settings → About phone** and tap **Build number** seven times.
2. Open **Developer options** and turn on **USB debugging**.
3. Plug the phone into the PC with a data cable.
4. Unlock the phone and tap **Allow** on the USB debugging prompt.
5. In Luma, click **Refresh phones**, select the phone, and click **Start**.

Click the picture to tap. Drag to swipe. Scroll the wheel to scroll. Type to send text. Esc or right-click goes back.

## What you can do

- Back, Home, Recents, volume, power, rotate
- Picture: Fast, 720p, 1080p, YouTube. Nothing is encoded above 1080p (long side 1920).
- USB encodes H.264 with no B-frames (so the decoder is not waiting on future frames) and shows the newest picture instead of a queue.
- Screenshot (saved in `Pictures\Luma`)
- Record at the stream you picked (saved in `Videos\Luma`, ready to drop into YouTube)
- Paste your PC clipboard onto the phone
- Install an APK
- Send a file into the phone's Download folder
- Full screen
- Wireless: plug in once, type the phone's Wi-Fi address (or leave it blank), click **Connect wireless**, then unplug

## Notes

The phone must stay unlocked for the picture to keep updating. USB debugging has to stay on.

Some phones label the USB mode as **File transfer** or **MTP**. That is fine as long as debugging is allowed.

The first launch of a lone `Luma.exe` downloads a small USB bridge, a video decoder, and the on-phone capture service. After that they stay in `%LOCALAPPDATA%\Luma\tools`. The zip already includes them.

## Build

```
go build -ldflags "-s -w -H windowsgui" -o Luma.exe ./cmd/luma
```

GitHub Actions builds the Windows release on every push to `main`.
