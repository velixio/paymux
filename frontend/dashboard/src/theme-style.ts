import { getNonce } from "get-nonce";

/**
 * Puts css in the page's <style id>. The browser checks an inline style against
 * the Content Security Policy when it is inserted, so a new element gets its
 * nonce and its text first and is appended last. An existing element that does
 * not carry the current nonce is replaced rather than edited.
 */
export function writeStyle(id: string, css: string) {
  const nonce = getNonce();
  const old = document.getElementById(id);
  if (old && (!nonce || old.getAttribute("nonce") === nonce)) {
    old.textContent = css;
    return;
  }
  const tag = document.createElement("style");
  tag.id = id;
  if (nonce) tag.setAttribute("nonce", nonce);
  tag.textContent = css;
  old?.remove();
  document.head.append(tag);
}
