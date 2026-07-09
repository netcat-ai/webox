# Bundled WeChat

Put the official Linux WeChat deb here before building the image:

- `WeChatLinux_x86_64.deb` for amd64
- `WeChatLinux_arm64.deb` for arm64

Download from the official Linux WeChat page:

- https://linux.weixin.qq.com/
- `https://dldir1v6.qq.com/weixin/Universal/Linux/WeChatLinux_x86_64.deb`
- `https://dldir1v6.qq.com/weixin/Universal/Linux/WeChatLinux_arm64.deb`

The container does not download or update WeChat at runtime. Rebuild the image to change the bundled WeChat version.
Local package files are ignored by git.
