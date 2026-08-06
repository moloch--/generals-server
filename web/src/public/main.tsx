import {StrictMode} from "react";
import {createRoot} from "react-dom/client";

import {PublicApp} from "./PublicApp";
import "./index.css";
import {PublicRouter} from "./router";

const root = document.getElementById("root");

if (!root) {
  throw new Error("Public application root is missing.");
}

createRoot(root).render(
  <StrictMode>
    <PublicRouter>
      <PublicApp />
    </PublicRouter>
  </StrictMode>,
);
