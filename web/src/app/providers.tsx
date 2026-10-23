"use client";

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createContext, useContext, useEffect, useState } from "react";
import { useUI } from "@/lib/ui-store";

const TimeContext = createContext(0);

export function useNow() {
  return useContext(TimeContext);
}

export function Providers({ children }: { children: React.ReactNode }) {
  const [client] = useState(() => new QueryClient());
  const [now, setNow] = useState(0);
  const theme = useUI((state) => state.theme);

  useEffect(() => {
    void useUI.persist.rehydrate();
    let timer: number | undefined;
    const updateVisibility = () => {
      window.clearInterval(timer);
      timer = undefined;
      if (document.visibilityState === "visible") {
        setNow(Date.now());
        timer = window.setInterval(() => setNow(Date.now()), 15_000);
      }
    };
    updateVisibility();
    document.addEventListener("visibilitychange", updateVisibility);
    return () => {
      window.clearInterval(timer);
      document.removeEventListener("visibilitychange", updateVisibility);
    };
  }, []);

  useEffect(() => {
    document.documentElement.dataset.theme = theme;
    document.documentElement.style.colorScheme = theme;
  }, [theme]);

  return (
    <QueryClientProvider client={client}>
      <TimeContext.Provider value={now}>{children}</TimeContext.Provider>
    </QueryClientProvider>
  );
}
