"""PyInstaller entry point; keeps package-relative imports intact."""

from mcloudmount.cli import main

if __name__ == "__main__":
    raise SystemExit(main())
