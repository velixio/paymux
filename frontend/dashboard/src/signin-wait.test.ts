import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { startSigninWait } from "./signin-wait";

const answers = (...bodies: (string | Error | number)[]) => {
  let i = 0;
  return vi.fn(async () => {
    const next = bodies[Math.min(i++, bodies.length - 1)];
    if (next instanceof Error) throw next;
    if (typeof next === "number") return new Response("oops", { status: next });
    return new Response(next);
  }) as unknown as typeof fetch;
};

beforeEach(() => {
  vi.useFakeTimers();
});
afterEach(() => {
  vi.useRealTimers();
});

test("waits while the answer is pending", async () => {
  const done = vi.fn();
  const stop = startSigninWait("/s", done, answers("pending"));
  await vi.advanceTimersByTimeAsync(6000);
  expect(done).not.toHaveBeenCalled();
  stop();
});

test("finishes once when the answer is ready, then stops asking", async () => {
  const done = vi.fn();
  const fetcher = answers("pending", "ready");
  startSigninWait("/s", done, fetcher);
  await vi.advanceTimersByTimeAsync(10000);
  expect(done).toHaveBeenCalledTimes(1);
  expect(fetcher).toHaveBeenCalledTimes(2);
  expect(fetcher).toHaveBeenCalledWith("/s", {
    credentials: "same-origin",
  });
});

test("keeps asking after a network error or a server error", async () => {
  const done = vi.fn();
  startSigninWait("/s", done, answers(new Error("offline"), 500, "ready"));
  await vi.advanceTimersByTimeAsync(2000);
  expect(done).not.toHaveBeenCalled();
  await vi.advanceTimersByTimeAsync(2000);
  expect(done).not.toHaveBeenCalled();
  await vi.advanceTimersByTimeAsync(2000);
  expect(done).toHaveBeenCalledTimes(1);
});

test("the stop function ends the polling", async () => {
  const done = vi.fn();
  const fetcher = answers("pending");
  const stop = startSigninWait("/s", done, fetcher);
  await vi.advanceTimersByTimeAsync(2000);
  stop();
  const calls = vi.mocked(fetcher).mock.calls.length;
  await vi.advanceTimersByTimeAsync(10000);
  expect(vi.mocked(fetcher).mock.calls.length).toBe(calls);
});
