"use client";

import * as React from "react";

export default function LegacyLoginRedirect() {
  React.useEffect(() => {
    window.location.replace("/welcome");
  }, []);

  return null;
}
