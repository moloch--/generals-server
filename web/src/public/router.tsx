import {
  type ComponentPropsWithoutRef,
  createContext,
  type MouseEvent,
  type PropsWithChildren,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
} from "react";

export const publicRoutes = [
  "/",
  "/leaderboard",
  "/game-lobbies",
  "/online-players",
  "/active-games",
  "/how-to-play",
] as const;

export type PublicRoute = typeof publicRoutes[number];

interface PublicRouterState {
  navigate: (route: PublicRoute) => void;
  route: PublicRoute;
}

const PublicRouterContext = createContext<PublicRouterState | null>(null);

function isPublicRoute(pathname: string): pathname is PublicRoute {
  return publicRoutes.some((route) => route === pathname);
}

function readRoute(): PublicRoute {
  return isPublicRoute(window.location.pathname) ? window.location.pathname : "/";
}

export function PublicRouter({children}: PropsWithChildren) {
  const [route, setRoute] = useState<PublicRoute>(readRoute);

  useEffect(() => {
    const handlePopState = () => setRoute(readRoute());
    window.addEventListener("popstate", handlePopState);
    return () => window.removeEventListener("popstate", handlePopState);
  }, []);

  const navigate = useCallback((nextRoute: PublicRoute) => {
    if (window.location.pathname !== nextRoute || window.location.search || window.location.hash) {
      window.history.pushState(null, "", nextRoute);
    }
    setRoute(nextRoute);
  }, []);

  const value = useMemo(() => ({navigate, route}), [navigate, route]);
  return <PublicRouterContext.Provider value={value}>{children}</PublicRouterContext.Provider>;
}

export function usePublicRouter(): PublicRouterState {
  const router = useContext(PublicRouterContext);
  if (!router) {
    throw new Error("Public router is missing.");
  }
  return router;
}

function shouldHandleRouteClick(event: MouseEvent<HTMLAnchorElement>): boolean {
  return !event.defaultPrevented
    && event.button === 0
    && !event.altKey
    && !event.ctrlKey
    && !event.metaKey
    && !event.shiftKey
    && event.currentTarget.target !== "_blank"
    && !event.currentTarget.hasAttribute("download");
}

interface PublicRouteLinkProps extends Omit<ComponentPropsWithoutRef<"a">, "href"> {
  to: PublicRoute;
}

export function PublicRouteLink({children, onClick, to, ...props}: PublicRouteLinkProps) {
  const {navigate} = usePublicRouter();

  return (
    <a
      {...props}
      href={to}
      onClick={(event) => {
        onClick?.(event);
        if (shouldHandleRouteClick(event)) {
          event.preventDefault();
          navigate(to);
        }
      }}
    >
      {children}
    </a>
  );
}
