"use strict";
const ru = navigator.language.toLowerCase().startsWith("ru");
document.documentElement.lang = ru ? "ru" : "en";
const message = document.getElementById("message");
const button = document.getElementById("connect");
button.textContent = ru ? "Подключить YouTube" : "Connect YouTube";
message.textContent = ru
  ? "Передать сессию YouTube медиасерверу на этом компьютере. Пароли и данные других сайтов не передаются."
  : "Share your YouTube session with the media server on this computer. Passwords and data from other sites are not shared.";
button.addEventListener("click", async () => {
  button.disabled = true;
  try {
    const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
    const target = new URL(tab?.url || "about:blank");
    if (target.protocol !== "http:" || target.hostname !== "127.0.0.1" || !target.port ||
        target.username || target.password || target.pathname !== "/youtube-connect" || !/^#[a-f0-9]{64}$/.test(target.hash)) {
      throw new Error(ru ? "Откройте Linn и нажмите «Войти через браузер». На открывшейся странице снова нажмите значок расширения." : "Open Linn and click Sign in with browser. On the page that opens, click this extension again.");
    }
    const cookies = (await chrome.cookies.getAll({ domain: "youtube.com" })).filter(cookie =>
      [".youtube.com", "youtube.com", "www.youtube.com"].includes(cookie.domain));
    if (!cookies.some(cookie => ["SAPISID", "__Secure-3PAPISID"].includes(cookie.name) && cookie.value)) {
      throw new Error(ru ? "Сначала войдите в YouTube в этом браузере, затем вернитесь на страницу подключения." : "Sign in to YouTube in this browser, then return to the connection page.");
    }
    message.textContent = ru ? "Подключение…" : "Connecting…";
    const response = await fetch(`${target.origin}/youtube-session`, {
      method: "POST", redirect: "error", credentials: "omit",
      headers: { "Content-Type": "application/json", "Authorization": `Bearer ${target.hash.slice(1)}` },
      body: JSON.stringify(cookies.map(({ domain, name, value, path, secure, expirationDate }) =>
        ({ domain, name, value, path, secure, expirationDate }))),
      signal: AbortSignal.timeout(10000)
    });
    if (!response.ok) throw new Error(ru ? "Подключение не завершено. Войдите в YouTube и начните подключение заново в Linn." : "Connection failed. Sign in to YouTube and start a new connection in Linn.");
    message.textContent = ru ? "Подключено. Linn загружает ваши списки. Браузер можно закрыть." : "Connected. Linn is loading your lists. You can close the browser.";
  } catch (error) {
    message.textContent = error instanceof TypeError || error.name === "TimeoutError"
      ? (ru ? "Сервер недоступен. Начните подключение заново в Linn." : "Server unavailable. Start a new connection in Linn.")
      : error.message;
    button.disabled = false;
  }
});
