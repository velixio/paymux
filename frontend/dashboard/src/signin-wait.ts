/**
 * Asks statusUrl every everyMs until it answers "ready", then calls done once.
 * A failed request only means asking again. Returns a function that stops it.
 */
export function startSigninWait(
  statusUrl: string,
  done: () => void,
  fetcher: typeof fetch = (...args) => fetch(...args),
  everyMs = 2000,
): () => void {
  let stopped = false;
  let timer: ReturnType<typeof setTimeout> | undefined;
  const ask = async () => {
    let ready = false;
    try {
      const response = await fetcher(statusUrl, {
        credentials: "same-origin",
      });
      ready = response.ok && (await response.text()).trim() === "ready";
    } catch {
      // Offline for a moment: the next attempt decides.
    }
    if (stopped) return;
    if (ready) {
      done();
      return;
    }
    timer = setTimeout(ask, everyMs);
  };
  timer = setTimeout(ask, everyMs);
  return () => {
    stopped = true;
    clearTimeout(timer);
  };
}
