"use client";

import { Suspense } from "react";
import { useSearchParams } from "next/navigation";
import { InfoFlowBindPage } from "@multica/views/infoflow";

// /infoflow/bind?token=<raw> is the InfoFlow robot's "you're not bound yet,
// click here" destination. Mirrors /lark/bind and /wecom/bind: the page reads
// the one-shot token from the query string and hands it to the shared view,
// which redeems it against the logged-in Multica session.
function InfoFlowBindPageContent() {
  const params = useSearchParams();
  const token = params.get("token");
  return <InfoFlowBindPage token={token} />;
}

export default function Page() {
  return (
    <Suspense fallback={null}>
      <InfoFlowBindPageContent />
    </Suspense>
  );
}
