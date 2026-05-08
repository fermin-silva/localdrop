# LocalDrop

Transfer files from your phone to your computer over the local network. No cables, no cloud, no accounts.

## How it works

LocalDrop starts a small HTTP server on your computer, then displays a QR code in your terminal. Scan it with your phone and you get a simple upload page. Files stream directly to disk with minimal memory usage.

## Install

### Download binary (recommended)

Go to the [Releases](https://github.com/fermin-silva/localdrop/releases) page and download the binary for your platform.

On macOS, double-click the downloaded file — a terminal window will open, the server will start, and a QR code will appear for you to scan.

### Build from source

Requires [Go](https://go.dev/dl/) 1.21+.

```sh
git clone https://github.com/fermin-silva/localdrop
cd localdrop
go build -o localdrop .
./localdrop
```

## Usage

```sh
./localdrop
```

This will:
1. Detect your local network IP
2. Start an HTTP server on a random available port
3. Print a QR code you can scan with your phone

Open the URL on your phone (must be on the same Wi-Fi network), pick a file, and hit Upload.

Files are saved to the `localdrop_downloads/` folder in the current directory.

## Tips

- Keep the browser tab open and in the foreground during upload
- Both devices must be on the same local network
- Works with Safari, Chrome, and other mobile browsers
