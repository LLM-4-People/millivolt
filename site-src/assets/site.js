(function () {
  "use strict";

  const root = document.body.dataset.root || "";
  const navToggle = document.querySelector(".nav-toggle");
  const siteNav = document.getElementById("site-nav");

  if (navToggle && siteNav) {
    const setOpen = (open) => {
      navToggle.setAttribute("aria-expanded", String(open));
      navToggle.setAttribute("aria-label", open ? "Close menu" : "Open menu");
      siteNav.classList.toggle("open", open);
    };
    navToggle.addEventListener("click", () => {
      setOpen(navToggle.getAttribute("aria-expanded") !== "true");
    });
    siteNav.addEventListener("click", (event) => {
      if (event.target.closest("a")) setOpen(false);
    });
    document.addEventListener("keydown", (event) => {
      if (event.key === "Escape" && navToggle.getAttribute("aria-expanded") === "true") {
        setOpen(false);
        navToggle.focus();
      }
    });
  }

  document.querySelectorAll(".doc-content table").forEach((table) => {
    if (table.parentElement && table.parentElement.classList.contains("table-scroll")) return;
    const wrapper = document.createElement("div");
    wrapper.className = "table-scroll";
    table.parentNode.insertBefore(wrapper, table);
    wrapper.appendChild(table);
  });

  const docsNav = document.querySelector(".docs-nav");
  const toc = document.querySelector(".toc");
  if (docsNav || toc) {
    // Both rails are disclosures: always open while they are side rails, closed
    // while they are inline panels above the article, so a phone lands on the
    // page title instead of a 30-item list.
    const wide = window.matchMedia("(max-width: 1180px)");
    const syncRails = () => {
      if (docsNav) docsNav.open = !wide.matches;
      if (toc) toc.open = !wide.matches;
    };
    syncRails();
    wide.addEventListener("change", syncRails);
  }

  const progress = document.querySelector(".read-progress");
  if (progress) {
    let queued = false;
    const paint = () => {
      queued = false;
      const doc = document.documentElement;
      const span = doc.scrollHeight - doc.clientHeight;
      const ratio = span > 0 ? Math.min(1, Math.max(0, doc.scrollTop / span)) : 0;
      progress.style.transform = `scaleX(${ratio})`;
    };
    const schedule = () => {
      if (queued) return;
      queued = true;
      window.requestAnimationFrame(paint);
    };
    paint();
    document.addEventListener("scroll", schedule, { passive: true });
    window.addEventListener("resize", schedule);
  }

  // The landing page's console miniature is a real tablist, so a visitor can
  // read every section without a screenshot. The real dashboard owns the same
  // keys: arrows move selection and focus, Home and End jump.
  const consolePlate = document.querySelector(".console-plate");
  const plateTabs = consolePlate ? Array.from(consolePlate.querySelectorAll(".cp-rail [role=tab]")) : [];
  if (plateTabs.length) {
    const selectPlate = (tab, focus) => {
      for (const other of plateTabs) {
        const on = other === tab;
        other.setAttribute("aria-selected", String(on));
        other.tabIndex = on ? 0 : -1;
        const panel = document.getElementById(other.getAttribute("aria-controls"));
        if (panel) panel.hidden = !on;
      }
      if (focus) tab.focus();
    };
    plateTabs.forEach((tab, index) => {
      tab.addEventListener("click", () => selectPlate(tab, false));
      tab.addEventListener("keydown", (event) => {
        const step = event.key === "ArrowDown" || event.key === "ArrowRight" ? 1
          : event.key === "ArrowUp" || event.key === "ArrowLeft" ? -1 : 0;
        const jump = event.key === "Home" || event.key === "End";
        if (!step && !jump) return;
        event.preventDefault();
        const next = jump
          ? plateTabs[event.key === "Home" ? 0 : plateTabs.length - 1]
          : plateTabs[(index + step + plateTabs.length) % plateTabs.length];
        selectPlate(next, true);
      });
    });
  }

  const live = document.createElement("span");
  live.className = "sr-only";
  live.setAttribute("aria-live", "polite");
  document.body.appendChild(live);

  const copyText = async (text) => {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(text);
      return;
    }
    const area = document.createElement("textarea");
    area.value = text;
    area.setAttribute("readonly", "");
    area.style.position = "fixed";
    area.style.opacity = "0";
    document.body.appendChild(area);
    area.select();
    const copied = document.execCommand("copy");
    area.remove();
    if (!copied) throw new Error("copy failed");
  };

  const markCopied = (button) => {
    const original = button.textContent;
    button.textContent = "copied";
    button.disabled = true;
    live.textContent = "Copied to clipboard.";
    window.setTimeout(() => {
      button.textContent = original;
      button.disabled = false;
    }, 1500);
  };

  document.querySelectorAll("[data-copy]").forEach((button) => {
    button.addEventListener("click", async () => {
      const scope = button.closest(".code-window");
      const code = scope && scope.querySelector("code");
      if (!code) return;
      try {
        await copyText(code.textContent);
        markCopied(button);
      } catch {
        live.textContent = "Copy failed. Select the command and copy it manually.";
      }
    });
  });

  document.querySelectorAll(".doc-content pre").forEach((pre) => {
    if (pre.querySelector(".copy-btn") || pre.querySelector('[data-copy]')) return;
    const button = document.createElement("button");
    button.type = "button";
    button.className = "copy-btn";
    button.textContent = "copy";
    button.setAttribute("aria-label", "Copy code block");
    button.addEventListener("click", async () => {
      const code = pre.querySelector("code");
      if (!code) return;
      try {
        await copyText(code.textContent);
        markCopied(button);
      } catch {
        live.textContent = "Copy failed. Select the code and copy it manually.";
      }
    });
    pre.appendChild(button);
  });

  const tocLinks = Array.from(document.querySelectorAll(".toc-list a"));
  if (tocLinks.length && "IntersectionObserver" in window) {
    const headings = Array.from(document.querySelectorAll(".doc-content h2[id], .doc-content h3[id]"));
    const byId = new Map(tocLinks.map((link) => [link.getAttribute("href").slice(1), link]));
    const observer = new IntersectionObserver((entries) => {
      entries.forEach((entry) => {
        if (!entry.isIntersecting) return;
        tocLinks.forEach((link) => {
          link.classList.remove("active");
          link.removeAttribute("aria-current");
        });
        const link = byId.get(entry.target.id);
        if (link) {
          link.classList.add("active");
          link.setAttribute("aria-current", "location");
        }
      });
    }, { rootMargin: "-100px 0px -70% 0px", threshold: 0 });
    headings.forEach((heading) => observer.observe(heading));
    // Before any scroll, name where the reader is: the first section, unless a
    // fragment already points at one.
    const initial = tocLinks.find((link) => "#" + link.getAttribute("href").slice(1) === window.location.hash)
      || tocLinks[0];
    initial.classList.add("active");
    initial.setAttribute("aria-current", "location");
  }

  const dialog = document.getElementById("search-dialog");
  const input = document.getElementById("search-input");
  const results = document.getElementById("search-results");
  const empty = document.getElementById("search-empty");
  let indexPromise = null;
  let index = null;

  const loadIndex = () => {
    if (!indexPromise) {
      indexPromise = fetch(root + "search.json", { credentials: "same-origin" })
        .then((response) => {
          if (!response.ok) throw new Error("search index unavailable");
          return response.json();
        })
        .then((data) => {
          index = data;
          return data;
        })
        .catch((error) => {
          indexPromise = null;
          throw error;
        });
    }
    return indexPromise;
  };

  const resultFor = (entry, term) => {
    const item = document.createElement("li");
    const link = document.createElement("a");
    link.href = root + entry.url;
    const title = document.createElement("strong");
    title.textContent = entry.title;
    link.appendChild(title);
    const heading = (entry.headings || []).find((item) => item.text.toLowerCase().includes(term));
    if (heading && heading.id) link.href = root + entry.url + "#" + heading.id;
    const detail = document.createElement("span");
    detail.textContent = heading ? heading.text : entry.description;
    link.appendChild(detail);
    item.appendChild(link);
    return item;
  };

  const search = (query) => {
    if (!results || !empty) return;
    results.replaceChildren();
    const term = query.trim().toLowerCase();
    if (!term) {
      empty.hidden = true;
      live.textContent = "";
      return;
    }
    if (!index) {
      loadIndex().then(() => search(query)).catch(() => {
        empty.hidden = false;
        empty.textContent = "The search index could not be loaded. Browse the docs instead.";
      });
      return;
    }
    const terms = term.split(/\s+/).filter(Boolean);
    const matches = [];
    for (const entry of index) {
      const headings = (entry.headings || []).map((item) => item.text).join(" ");
      const haystack = [entry.title, entry.description, headings, entry.text].join(" ").toLowerCase();
      if (!terms.every((part) => haystack.includes(part))) continue;
      let score = 0;
      if (entry.title.toLowerCase().includes(term)) score += 8;
      if (headings.toLowerCase().includes(term)) score += 4;
      if (entry.description.toLowerCase().includes(term)) score += 2;
      matches.push({ entry, score });
    }
    matches.sort((a, b) => b.score - a.score || a.entry.title.localeCompare(b.entry.title));
    const shown = matches.slice(0, 12);
    shown.forEach(({ entry }) => results.appendChild(resultFor(entry, term)));
    empty.hidden = matches.length > 0;
    if (!matches.length) empty.textContent = "No matching page. Try a shorter term or browse the docs.";
    live.textContent = !matches.length
      ? "No matching page."
      : matches.length > shown.length
        ? `Showing ${shown.length} of ${matches.length} results.`
        : `${matches.length} result${matches.length === 1 ? "" : "s"}.`;
  };

  const openSearch = async () => {
    if (!dialog) return;
    if (typeof dialog.showModal === "function") dialog.showModal();
    else dialog.setAttribute("open", "");
    if (input) {
      input.value = "";
      input.focus();
    }
    if (results) results.replaceChildren();
    if (empty) empty.hidden = true;
    live.textContent = "";
    if (input) search("");
    loadIndex().catch(() => {
      if (empty) {
        empty.hidden = false;
        empty.textContent = "The search index could not be loaded. Browse the docs instead.";
      }
    });
  };

  document.querySelectorAll("[data-search-open]").forEach((button) => {
    button.addEventListener("click", openSearch);
  });

  if (input) {
    let timer = 0;
    input.addEventListener("input", () => {
      window.clearTimeout(timer);
      timer = window.setTimeout(() => search(input.value), 90);
    });
  }

  if (dialog) {
    dialog.addEventListener("click", (event) => {
      if (event.target === dialog) dialog.close();
    });
  }

  document.addEventListener("keydown", (event) => {
    if (event.defaultPrevented || event.metaKey || event.ctrlKey || event.altKey) return;
    const target = event.target;
    const typing = target && (
      target.tagName === "INPUT" || target.tagName === "TEXTAREA" ||
      target.tagName === "SELECT" || target.isContentEditable
    );
    if (event.key === "/" && !typing) {
      event.preventDefault();
      openSearch();
    }
  });
})();
