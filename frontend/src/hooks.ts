import { useEffect, useState } from "react";

// useAsync runs loader and tracks {data,error}. It re-runs whenever a value in
// deps changes; the in-flight request is aborted on re-run or unmount.
export function useAsync<T>(loader: (s: AbortSignal) => Promise<T>, deps: unknown[] = []): { data: T | null; error: string } {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState("");
  useEffect(() => {
    const ac = new AbortController();
    loader(ac.signal).then(setData).catch((e) => { if (e.name !== "AbortError") setError(String(e)); });
    return () => ac.abort();
  }, deps); // eslint-disable-line react-hooks/exhaustive-deps
  return { data, error };
}
