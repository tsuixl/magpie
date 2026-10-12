// Sync to an S3 bucket from Settings (#296): WebDAV or S3 picked in the
// form, the bucket's fields posted as the server takes them (s3://bucket/
// prefix, the access key as the user and its secret as the password), the
// row saying S3 once on, and what was typed for one kind kept on going to
// the other and back. The API is faked; nothing is synced.
const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");

const assets = process.env.MAGPIE_SYNC_ASSETS || path.resolve(__dirname, "../assets");

function serve(lang, posts, initialSync) {
  const settings = {
    theme: "light", lang, tray: "panel", version: "test", dir: "/tmp/magpie",
    gateway: "http://127.0.0.1:3425", visionModels: [], imageGenModels: [],
    fx: { rate: 7.2, stale: false },
  };
  let sync = initialSync || { on: false, keys: true, agents: true, library: true };
  return async (route) => {
    const request = route.request();
    const pathname = new URL(request.url()).pathname;
    const json = (data) => route.fulfill({ json: data });
    if (pathname === "/boot.js") return route.fulfill({ contentType: "text/javascript", body: `window.bootPrefs = {lang:"${lang}",theme:"light",web:false};` });
    if (pathname === "/wails/runtime.js") return route.fulfill({ contentType: "text/javascript", body: "export const Window = {};" });
    if (pathname === "/api/state") return json({ agents: [], profiles: [], settings });
    if (pathname === "/api/settings") return json(settings);
    if (pathname === "/api/usage/quotas") return json([]);
    if (pathname === "/api/groups") return json({ groups: [], models: [] });
    if (pathname === "/api/plugins") return json({ plugins: [] });
    if (pathname === "/api/davsync") return json(sync);
    if (pathname === "/api/davsync/save") {
      const b = request.postDataJSON();
      posts.push(b);
      const target = (v) => v.kind === "s3" ? ((v.endpoint || "").startsWith("https://s3.oss-") ? "oss" : "s3") : "webdav";
      const saved = { ...(sync.saved || {}) };
      if (sync.on) saved[target(sync)] = { ...sync, saved: undefined };
      const nextTarget = b.url.startsWith("s3://") ? ((b.endpoint || "").startsWith("https://s3.oss-") ? "oss" : "s3") : "webdav";
      delete saved[nextTarget];
      sync = { on: true, kind: b.url.startsWith("s3://") ? "s3" : "webdav", url: b.url, user: b.user, endpoint: b.endpoint, region: b.region,
        saved, pathStyle: b.pathStyle, passwordSet: true, passphraseSet: true, keys: b.keys, agents: b.agents, library: b.library };
      return json(sync);
    }
    if (pathname.startsWith("/api/")) return json({});
    const file = path.join(assets, pathname === "/" ? "index.html" : pathname);
    const contentType = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".png": "image/png" }[path.extname(file)];
    return route.fulfill({ body: await fs.readFile(file), contentType });
  };
}

for (const engine of (process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"])) {
  for (const lang of ["en", "zh"]) {
    test(`${engine} ${lang}: S3 sync settings`, async (t) => {
      const zh = lang === "zh";
      const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
      const context = await browser.newContext({ viewport: { width: 900, height: 420 }, reducedMotion: "reduce" });
      const page = await context.newPage();
      const errors = [], posts = [];
      page.on("pageerror", (error) => errors.push(error.message));
      await page.route("**/*", serve(lang, posts));
      t.after(async () => {
        if (process.env.ARTIFACT_DIR) {
          await fs.mkdir(process.env.ARTIFACT_DIR, { recursive: true });
          await page.screenshot({ path: path.join(process.env.ARTIFACT_DIR, `${engine}-${lang}-s3-sync.png`), fullPage: true });
        }
        await browser.close();
      });

      await page.goto("http://magpie.test/");
      await page.locator("#prefs").click();
      await page.locator(":is(#setTab-sync, .nav-s[data-of=setTab-sync]):visible").click();
      const list = page.locator("#syncList");
      const first = list.locator(".row.pref").first();
      await first.waitFor({ state: "visible" });
      // the sync rows are near the settings page's end: scrolled there by a
      // real wheel (the page keeps its place from any other scroll)
      const toEnd = async () => {
        await page.mouse.move(450, 300);
        const left = () => page.locator("#view-settings").evaluate((v) => v.scrollHeight - v.clientHeight - v.scrollTop);
        for (let i = 0; i < 120 && (await left()) > 0.5; i++) { await page.mouse.wheel(0, 80); await page.waitForTimeout(15); }
        await page.waitForTimeout(300);
      };
      await toEnd();
      const offName = await first.locator(".name").textContent();
      await first.locator(".val button").click();

      const form = list.locator(".sync-form");
      await form.waitFor({ state: "visible" });
      assert.equal(await form.locator(".segs .opt", { hasText: /^OSS$/ }).count(), 1, "OSS must be directly selectable beside WebDAV and S3");
      assert.deepEqual(await page.evaluate(() => [
        "https://s3.oss-cn-hangzhou-internal.aliyuncs.com", "https://s3.oss-accelerate.aliyuncs.com",
        "https://s3.oss-cn-hangzhou.aliyuncs.com.invalid", "https://s3.oss-invalid.aliyuncs.com",
      ].map(ossRegionOf)), ["cn-hangzhou", "", "", ""]);
      assert.equal(offName, zh ? "WebDAV / S3 / OSS 同步" : "WebDAV or S3 or OSS sync");
      const labels = async () => (await form.locator(":scope > label:visible").allTextContents()).filter(Boolean);
      // WebDAV is the one picked at first, S3's fields not shown
      assert.deepEqual(await labels(), zh ? ["同步到", "地址", "用户", "密码", "口令", "同时同步"] : ["Sync to", "Address", "User", "Password", "Passphrase", "Also sync"]);
      await form.locator(":scope > div").nth(1).locator("input").fill("https://dav.example.com/dav/");

      // picking S3 moves nothing on the page
      const top = () => page.evaluate(() => document.querySelector("#view-settings").scrollTop);
      // at the page's end, then back up by the wheel till the choice is in sight
      await toEnd();
      const segTop = () => page.evaluate(() => document.querySelector(".sync-form .segs").getBoundingClientRect().top - document.querySelector("#view-settings").getBoundingClientRect().top);
      for (let i = 0; i < 40 && (await segTop()) < 10; i++) { await page.mouse.wheel(0, -40); await page.waitForTimeout(15); }
      await page.waitForTimeout(300);
      const before = await top();
      assert.ok(before > 0, "the settings page must be scrolled");
      await form.locator(".segs .opt", { hasText: "S3" }).click();
      await page.waitForTimeout(250);
      assert.equal(await top(), before, "picking S3 must not scroll the page");
      assert.deepEqual(await labels(), zh
        ? ["同步到", "终结点", "存储桶", "前缀", "区域", "Access Key", "Secret", "口令", "同时同步"]
        : ["Sync to", "Endpoint", "Bucket", "Prefix", "Region", "Access key", "Secret", "Passphrase", "Also sync"]);
      // every label fits its column
      for (const l of await form.locator(":scope > label:visible").all()) {
        assert.equal(await l.evaluate((e) => e.scrollWidth <= e.clientWidth + 1), true, await l.textContent());
      }

      const box = (label) => form.locator(":scope > label:visible", { hasText: new RegExp("^" + label + "$") }).locator("xpath=following-sibling::div[1]").locator("input");
      const L = zh ? { endpoint: "终结点", bucket: "存储桶", prefix: "前缀", region: "区域", key: "Access Key", secret: "Secret", phrase: "口令" }
        : { endpoint: "Endpoint", bucket: "Bucket", prefix: "Prefix", region: "Region", key: "Access key", secret: "Secret", phrase: "Passphrase" };
      await box(L.endpoint).fill("https://acct.r2.cloudflarestorage.com");
      await box(L.phrase).fill("correct horse");
      const save = form.locator(".bar .primary");
      await save.click();
      assert.equal(await form.locator(".editor-error").textContent(), zh ? "请填写存储桶名称" : "Name the bucket");
      assert.equal(posts.length, 0);

      await box(L.bucket).fill("magpie-sync");
      await box(L.prefix).fill("/team/");
      await box(L.region).fill("auto");
      await box(L.key).fill("AKIDEXAMPLE");
      await box(L.secret).fill("s3cr3t");
      await form.locator("label.tick").first().click(); // path-style
      // the WebDAV address typed before is still there, and isn't sent;
      // going back and forth, the shorter form too, moves nothing
      // the fields filled scrolled the choice out of sight: wheeled back to it
      await page.mouse.move(450, 300);
      for (let i = 0; i < 40 && (await segTop()) < 10; i++) { await page.mouse.wheel(0, -40); await page.waitForTimeout(15); }
      await page.waitForTimeout(300);
      const at = await top();
      assert.ok(at > 0, "the settings page must still be scrolled");
      await form.locator(".segs .opt", { hasText: "WebDAV" }).click();
      await page.waitForTimeout(250);
      assert.equal(await top(), at, "picking WebDAV must not scroll the page");
      assert.equal(await form.locator(":scope > div").nth(1).locator("input").inputValue(), "https://dav.example.com/dav/");
      await form.locator(".segs .opt", { hasText: "S3" }).click();
      await page.waitForTimeout(250);
      assert.equal(await top(), at, "picking S3 again must not scroll the page");
      await toEnd(); // the save button, at the form's foot, wheeled into sight
      await save.click();
      await form.waitFor({ state: "detached" });
      assert.deepEqual(posts, [{ url: "s3://magpie-sync/team", user: "AKIDEXAMPLE", password: "s3cr3t", endpoint: "https://acct.r2.cloudflarestorage.com",
        region: "auto", pathStyle: true, passphrase: "correct horse", keys: true, agents: true, library: true }]);

      assert.equal(await first.locator(".name").textContent(), zh ? "S3 同步" : "S3 sync");
      assert.equal(await first.locator(".sub").textContent(), (zh ? "尚未同步 · " : "Not synced yet · ") + "s3://magpie-sync/team · acct.r2.cloudflarestorage.com");

      // editing it again: S3 picked, its fields as saved, the secret kept
      const edit = first.locator(".val button").last();
      await toEnd();
      // with the page scrolled to its end the row can sit under the sticky
      // header: wheeled back until it is clear of it
      const under = () => edit.evaluate((b) => b.getBoundingClientRect().top < document.querySelector("header.top").getBoundingClientRect().bottom + 4);
      for (let i = 0; i < 40 && (await under()); i++) { await page.mouse.wheel(0, -60); await page.waitForTimeout(30); }
      await edit.click();
      await form.waitFor({ state: "visible" });
      assert.equal(await form.locator(".segs .opt.on").textContent(), zh ? "S3 · 使用中" : "S3 · on");
      assert.equal(await box(L.bucket).inputValue(), "magpie-sync");
      assert.equal(await box(L.prefix).inputValue(), "team");
      assert.equal(await box(L.region).inputValue(), "auto");
      assert.equal(await box(L.key).inputValue(), "AKIDEXAMPLE");
      assert.equal(await box(L.secret).inputValue(), "");
      assert.equal(await box(L.secret).getAttribute("placeholder"), zh ? "已保存 · 输入新的即可替换" : "saved · type a new one to replace it");
      assert.equal(await form.locator("label.tick input").first().isChecked(), true);

      await page.setViewportSize({ width: 520, height: 700 });
      // WebKit applies the sidebar's media rule to the new width at the next
      // frame, so measure after that frame
      await page.evaluate(() => new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r))));
      assert.equal(await form.evaluate((e) => e.scrollWidth > e.clientWidth), false);

      // Each entry has independent drafts; an R2 secret stays in S3.
      await box(L.secret).fill("only-for-r2");
      await form.locator(".segs .opt", { hasText: /^OSS$/ }).click();
      assert.equal(await box(L.endpoint).inputValue(), "https://s3.oss-cn-hangzhou.aliyuncs.com");
      assert.equal(await box(L.region).inputValue(), "cn-hangzhou");
      assert.equal(await box(L.secret).inputValue(), "");
      assert.equal(await box(L.prefix).inputValue(), "backups", "only a new OSS entry gets the default prefix");
      const directory = () => form.locator(":scope > label:visible", { hasText: new RegExp("^" + L.prefix + "$") }).locator("xpath=following-sibling::div[1]").locator(".hint");
      assert.equal(await directory().textContent(), zh ? "备份目录：backups/magpie/" : "Backup directory: backups/magpie/");
      if (process.env.ARTIFACT_DIR && zh) {
        await fs.mkdir(process.env.ARTIFACT_DIR, { recursive: true });
        await page.setViewportSize({ width: 900, height: 1100 });
        await page.waitForTimeout(300);
        await toEnd();
        await page.screenshot({ path: path.join(process.env.ARTIFACT_DIR, `${engine}-zh-oss-new-backups.png`), fullPage: true });
        await page.setViewportSize({ width: 520, height: 700 });
        await page.waitForTimeout(300);
      }
      await box(L.bucket).fill("oss-backups");
      await box(L.prefix).fill("team");
      assert.equal(await directory().textContent(), zh ? "备份目录：team/magpie/" : "Backup directory: team/magpie/");
      await box(L.key).fill("OSSKEY");
      await box(L.region).fill("cn-shanghai");
      assert.equal(await box(L.endpoint).inputValue(), "https://s3.oss-cn-shanghai.aliyuncs.com");
      await box(L.secret).fill("oss-secret");
      // Invalid OSS endpoints and a mismatched region are refused locally.
      for (const bad of ["", "https://oss-cn-shanghai.aliyuncs.com", "https://s3.oss-cn-shanghai.aliyuncs.com.invalid"]) {
        await box(L.endpoint).fill(bad);
        await toEnd();
        await save.click();
        assert.equal(await form.locator(".editor-error").textContent(), zh ? "请填写 OSS 区域 S3 终结点" : "Use a regional OSS S3 endpoint");
        assert.equal(posts.length, 1);
      }
      await box(L.endpoint).fill("https://s3.oss-cn-hangzhou.aliyuncs.com");
      await toEnd();
      await save.click();
      assert.equal(await form.locator(".editor-error").textContent(), zh ? "OSS 终结点和区域必须一致" : "The OSS endpoint and region must match");
      assert.equal(posts.length, 1);
      await box(L.endpoint).fill("https://s3.oss-cn-shanghai.aliyuncs.com");
      for (let i = 0; i < 40 && (await segTop()) < 10; i++) { await page.mouse.wheel(0, -40); await page.waitForTimeout(15); }
      await page.waitForTimeout(300);
      await form.locator(".segs .opt", { hasText: "WebDAV" }).click();
      await form.locator(".segs .opt", { hasText: "S3" }).click();
      assert.equal(await box(L.endpoint).inputValue(), "https://acct.r2.cloudflarestorage.com");
      assert.equal(await box(L.secret).inputValue(), "only-for-r2");
      await form.locator(".segs .opt", { hasText: /^OSS$/ }).click();
      assert.equal(await box(L.endpoint).inputValue(), "https://s3.oss-cn-shanghai.aliyuncs.com");
      assert.equal(await box(L.secret).inputValue(), "oss-secret");
      await toEnd();
      await save.click();
      await form.waitFor({ state: "detached" });
      assert.deepEqual(posts[1], { url: "s3://oss-backups/team", user: "OSSKEY", password: "oss-secret",
        endpoint: "https://s3.oss-cn-shanghai.aliyuncs.com", region: "cn-shanghai", pathStyle: false,
        passphrase: "", keys: true, agents: true, library: true });
      assert.equal(await first.locator(".name").textContent(), zh ? "OSS 同步" : "OSS sync");
      await page.waitForTimeout(300);
      await toEnd();
      for (let i = 0; i < 40 && (await under()); i++) { await page.mouse.wheel(0, -60); await page.waitForTimeout(30); }
      await first.locator(".val button").last().click();
      await form.waitFor({ state: "visible" });
      assert.equal(await form.locator(".segs .opt.on").textContent(), zh ? "OSS · 使用中" : "OSS · on");
      assert.equal(await box(L.bucket).inputValue(), "oss-backups");
      if (process.env.ARTIFACT_DIR && zh) {
        await fs.mkdir(process.env.ARTIFACT_DIR, { recursive: true });
        await page.setViewportSize({ width: 900, height: 1100 });
        await page.waitForTimeout(300);
        await toEnd();
        await page.screenshot({ path: path.join(process.env.ARTIFACT_DIR, `${engine}-zh-oss-selected.png`), fullPage: true });
      }
      await form.locator(".segs .opt", { hasText: /^S3$/ }).click();
      assert.equal(await box(L.endpoint).inputValue(), "https://acct.r2.cloudflarestorage.com");
      assert.equal(await box(L.key).inputValue(), "AKIDEXAMPLE");
      assert.equal(await box(L.secret).getAttribute("placeholder"), zh ? "已保存 · 输入新的即可替换" : "saved · type a new one to replace it");
      await form.locator(".segs .opt", { hasText: "OSS" }).click();
      await box(L.endpoint).fill("https://s3.oss-cn-shanghai-internal.aliyuncs.com");
      await box(L.secret).fill("oss-internal-secret");
      await toEnd();
      await save.click();
      await form.waitFor({ state: "detached" });
      assert.equal(posts[2].endpoint, "https://s3.oss-cn-shanghai-internal.aliyuncs.com");
      await page.waitForTimeout(300);
      await toEnd();
      for (let i = 0; i < 40 && (await under()); i++) { await page.mouse.wheel(0, -60); await page.waitForTimeout(30); }
      await first.locator(".val button").last().click();
      await form.waitFor({ state: "visible" });
      assert.equal(await box(L.endpoint).inputValue(), "https://s3.oss-cn-shanghai-internal.aliyuncs.com");
      await box(L.region).fill("cn-hangzhou");
      assert.equal(await box(L.endpoint).inputValue(), "https://s3.oss-cn-hangzhou-internal.aliyuncs.com");
      assert.deepEqual(errors, []);
    });
    test(`${engine} ${lang}: regional OSS endpoints with multiple region segments save`, async (t) => {
      const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
      t.after(() => browser.close());
      const page = await browser.newPage({ viewport: { width: 900, height: 1100 }, reducedMotion: "reduce" });
      const posts = [];
      await page.route("**/*", serve(lang, posts));
      await page.goto("http://magpie.test/");
      await page.locator("#prefs").click();
      await page.locator(":is(#setTab-sync, .nav-s[data-of=setTab-sync]):visible").click();
      const list = page.locator("#syncList"), first = list.locator(".row.pref").first(), form = list.locator(".sync-form");
      const words = lang === "zh" ? { endpoint: "终结点", bucket: "存储桶", region: "区域", key: "Access Key", secret: "Secret", phrase: "口令" }
        : { endpoint: "Endpoint", bucket: "Bucket", region: "Region", key: "Access key", secret: "Secret", phrase: "Passphrase" };
      const field = (name) => form.locator(":scope > label:visible", { hasText: new RegExp("^" + words[name] + "$") }).locator("xpath=following-sibling::div[1]").locator("input");
      for (const [region, internal] of [["cn-wuhan-lr", false], ["cn-wuhan-lr", true], ["cn-shanghai-finance-1", false], ["cn-shanghai-finance-1", true]]) {
        await first.locator(".val button").last().click();
        await form.waitFor({ state: "visible" });
        if (!posts.length) {
          await form.locator(".segs .opt", { hasText: /^OSS$/ }).click();
          await field("bucket").fill("test-bucket");
          await field("key").fill("SYNTHETICKEY");
          await field("secret").fill("dummy-secret");
          await field("phrase").fill("dummy passphrase");
        }
        const wasInternal = (await field("endpoint").inputValue()).includes("-internal.aliyuncs.com");
        await field("region").fill(region);
        assert.equal(await field("endpoint").inputValue(), `https://s3.oss-${region}${wasInternal ? "-internal" : ""}.aliyuncs.com`, "typing a valid multi-segment region updates the endpoint");
        const endpoint = `https://s3.oss-${region}${internal ? "-internal" : ""}.aliyuncs.com`;
        await field("endpoint").fill(endpoint);
        await form.locator(".bar .primary").click();
        await page.waitForTimeout(100);
        assert.equal(posts.at(-1)?.endpoint, endpoint, "a valid regional OSS endpoint must be submitted");
        assert.equal(posts.at(-1)?.region, region);
        await form.waitFor({ state: "detached" });
        assert.equal(await first.locator(".name").textContent(), lang === "zh" ? "OSS 同步" : "OSS sync");
      }
    });
    test(`${engine} ${lang}: an existing OSS empty prefix stays empty`, async (t) => {
      const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
      t.after(() => browser.close());
      const page = await browser.newPage({ viewport: { width: 900, height: 1100 }, reducedMotion: "reduce" });
      const posts = [];
      await page.route("**/*", serve(lang, posts, { on: true, kind: "s3", url: "s3://saved-oss", endpoint: "https://s3.oss-cn-hangzhou.aliyuncs.com",
        region: "cn-hangzhou", user: "SAVEDKEY", passwordSet: true, passphraseSet: true, keys: true, agents: true, library: true }));
      await page.goto("http://magpie.test/");
      await page.locator("#prefs").click();
      await page.locator(":is(#setTab-sync, .nav-s[data-of=setTab-sync]):visible").click();
      const list = page.locator("#syncList"), first = list.locator(".row.pref").first();
      await first.locator(".val button").last().click();
      const form = list.locator(".sync-form");
      const prefixField = form.locator(":scope > label:visible", { hasText: lang === "zh" ? /^前缀$/ : /^Prefix$/ }).locator("xpath=following-sibling::div[1]");
      assert.equal(await prefixField.locator("input").inputValue(), "");
      assert.equal(await prefixField.locator(".hint").textContent(), lang === "zh" ? "备份目录：magpie/" : "Backup directory: magpie/");
      await form.locator(".segs .opt", { hasText: /^S3$/ }).click();
      await form.locator(".segs .opt", { hasText: "OSS" }).click();
      assert.equal(await prefixField.locator("input").inputValue(), "");
      await form.locator(".bar .primary").click();
      await form.waitFor({ state: "detached" });
      assert.equal(posts[0].url, "s3://saved-oss");
    });
  }
}
