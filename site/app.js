(function () {
  "use strict";

  const byId = (id) => document.getElementById(id);

  function showToast(message) {
    const toast = byId("copy-toast");
    if (!toast) return;
    toast.textContent = message;
    toast.classList.add("visible");
    window.clearTimeout(showToast.timeoutId);
    showToast.timeoutId = window.setTimeout(() => toast.classList.remove("visible"), 1800);
  }

  async function copyText(text) {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(text);
      return;
    }

    const input = document.createElement("textarea");
    input.value = text;
    input.setAttribute("readonly", "");
    input.style.position = "fixed";
    input.style.opacity = "0";
    document.body.appendChild(input);
    input.select();
    const copied = document.execCommand("copy");
    input.remove();
    if (!copied) throw new Error("copy command failed");
  }

  document.querySelectorAll("[data-copy-target]").forEach((button) => {
    button.addEventListener("click", async () => {
      const target = byId(button.dataset.copyTarget);
      if (!target) return;
      try {
        await copyText(target.textContent.trim());
        showToast("명령을 복사했습니다.");
      } catch (_) {
        showToast("복사하지 못했습니다. 명령을 직접 선택해 주세요.");
      }
    });
  });

  function detectPlatform() {
    const platform = String(
      (navigator.userAgentData && navigator.userAgentData.platform) || navigator.platform || ""
    ).toLowerCase();
    const agent = String(navigator.userAgent || "").toLowerCase();

    if (platform.includes("mac") || agent.includes("macintosh")) {
      return {
        key: "darwin-arm64",
        label: "macOS 감지 · Apple Silicon 전용",
        warning: "브라우저는 Intel Mac을 정확히 구분하지 못할 수 있습니다. Apple Silicon에서만 사용하세요."
      };
    }
    if (platform.includes("linux") || agent.includes("linux")) {
      return {
        key: "linux-amd64",
        label: "Linux 감지 · Ubuntu 24.04 x86-64 전용",
        warning: "공식 지원 환경은 Ubuntu 24.04 LTS x86-64입니다."
      };
    }
    return {
      key: null,
      label: "지원 환경을 선택하세요",
      warning: "공식 지원: Apple Silicon macOS, Ubuntu 24.04 LTS x86-64"
    };
  }

  function asObject(value) {
    return value && typeof value === "object" && !Array.isArray(value) ? value : {};
  }

  function firstString(...values) {
    return values.find((value) => typeof value === "string" && value.trim()) || "";
  }

  function normalizeKey(key) {
    return String(key).toLowerCase().replaceAll("_", "-").replaceAll("/", "-");
  }

  function normalizeDownloads(metadata) {
    const root = asObject(metadata);
    const raw = root.downloads || root.artifacts || asObject(root.release).downloads || {};
    const result = {};

    if (Array.isArray(raw)) {
      raw.forEach((item) => {
        const value = asObject(item);
        const key = normalizeKey(firstString(value.target, value.platform, value.name));
        if (key) result[key] = value;
      });
      return result;
    }

    Object.entries(asObject(raw)).forEach(([key, value]) => {
      result[normalizeKey(key)] = typeof value === "string" ? { url: value } : asObject(value);
    });
    return result;
  }

  function findDownload(downloads, target) {
    const aliases = target === "darwin-arm64"
      ? ["darwin-arm64", "macos-arm64", "kit-darwin-arm64"]
      : ["linux-amd64", "ubuntu-amd64", "kit-linux-amd64"];
    for (const alias of aliases) {
      if (downloads[alias]) return downloads[alias];
    }
    return null;
  }

  function safeDownloadURL(value) {
    const url = firstString(value);
    if (!url) return "";
    try {
      const resolved = new URL(url, window.location.origin);
      if (resolved.protocol !== "https:" && resolved.origin !== window.location.origin) return "";
      return resolved.href;
    } catch (_) {
      return "";
    }
  }

  function normalizeNotes(metadata) {
    const root = asObject(metadata);
    const release = asObject(root.release);
    const raw = root.change_notes || root.changes || root.notes || root.changelog ||
      root.release_notes || release.notes || release.changes;
    if (Array.isArray(raw)) {
      return raw
        .map((item) => typeof item === "string" ? item : firstString(asObject(item).title, asObject(item).text))
        .filter(Boolean)
        .slice(0, 6);
    }
    if (typeof raw === "string") {
      return raw.split(/\r?\n/).map((line) => line.replace(/^[-*]\s*/, "").trim()).filter(Boolean).slice(0, 6);
    }
    return [];
  }

  function formatPublished(value) {
    if (!value) return "—";
    const date = new Date(value);
    if (Number.isNaN(date.getTime())) return String(value);
    return new Intl.DateTimeFormat("ko-KR", {
      year: "numeric", month: "short", day: "numeric", hour: "2-digit", minute: "2-digit",
      timeZoneName: "short"
    }).format(date);
  }

  function createDownloadButton(target, artifact, recommended) {
    const url = safeDownloadURL(firstString(artifact.url, artifact.href, artifact.download_url));
    if (!url) return null;

    const isMac = target === "darwin-arm64";
    const wrapper = document.createElement("div");
    const link = document.createElement("a");
    link.className = `button download-button${recommended ? " recommended" : " download-secondary"}`;
    link.href = url;
    link.setAttribute("download", "");
    link.textContent = `${isMac ? "macOS Apple Silicon" : "Ubuntu 24.04 x86-64"}${recommended ? " · 추천" : ""}`;
    wrapper.appendChild(link);

    const sha = firstString(artifact.sha256, artifact.checksum, artifact.sha);
    if (sha) {
      const checksum = document.createElement("span");
      checksum.className = "download-sha";
      checksum.title = `SHA-256 ${sha}`;
      checksum.textContent = `SHA-256 ${sha}`;
      wrapper.appendChild(checksum);
    }
    return wrapper;
  }

  function renderDownloads(metadata, platform) {
    const actions = byId("download-actions");
    if (!actions) return;
    const downloads = normalizeDownloads(metadata);
    const targets = platform.key
      ? [platform.key, platform.key === "darwin-arm64" ? "linux-amd64" : "darwin-arm64"]
      : ["darwin-arm64", "linux-amd64"];

    actions.replaceChildren();
    let count = 0;
    targets.forEach((target) => {
      const artifact = findDownload(downloads, target);
      if (!artifact) return;
      const button = createDownloadButton(target, artifact, platform.key === target);
      if (button) {
        actions.appendChild(button);
        count += 1;
      }
    });

    if (!count) {
      const fallback = document.createElement("a");
      fallback.className = "button download-button";
      fallback.href = "/release.json";
      fallback.textContent = "release.json 확인";
      actions.appendChild(fallback);
    }
  }

  function renderRelease(metadata, platform) {
    const root = asObject(metadata);
    const release = asObject(root.release);
    const version = firstString(root.version, release.version, root.tag, root.tag_name);
    const commit = firstString(root.build, root.build_commit, release.build, root.commit, release.commit);
    const published = firstString(root.published_at, root.publishedAt, release.published_at, release.publishedAt, root.created_at);

    byId("release-version").textContent = version || "version 미확인";
    byId("release-state").textContent = "배포됨";
    byId("release-build").textContent = commit ? commit.slice(0, 12) : "—";
    byId("release-build").title = commit;
    byId("release-published").textContent = formatPublished(published);

    const notes = normalizeNotes(root);
    const notesElement = byId("release-notes");
    if (notes.length) {
      notesElement.replaceChildren();
      notes.forEach((note) => {
        const item = document.createElement("li");
        item.textContent = note;
        notesElement.appendChild(item);
      });
    }

    renderDownloads(root, platform);
    byId("release-card").setAttribute("aria-busy", "false");
  }

  function renderReleaseError(platform) {
    byId("release-version").textContent = "확인할 수 없음";
    byId("release-state").textContent = "metadata 연결 실패";
    byId("release-state").classList.add("is-error");
    byId("release-card").setAttribute("aria-busy", "false");
    renderDownloads({}, platform);
  }

  const platform = detectPlatform();
  const detected = byId("detected-platform");
  const guidance = byId("download-guidance");
  if (detected) detected.textContent = platform.label;
  if (guidance) guidance.textContent = `${platform.warning} 일반 설치에는 위 설치 명령을 권장합니다.`;

  byId("release-version").textContent = "확인 중";
  byId("release-state").textContent = "릴리스 정보 조회 중";
  byId("release-card").setAttribute("aria-busy", "true");

  fetch("/release.json", { headers: { Accept: "application/json" }, cache: "no-cache" })
    .then((response) => {
      if (!response.ok) throw new Error(`release metadata: HTTP ${response.status}`);
      return response.json();
    })
    .then((metadata) => renderRelease(metadata, platform))
    .catch(() => renderReleaseError(platform));
})();
