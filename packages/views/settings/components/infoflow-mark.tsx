// InfoFlowMark renders the Baidu InfoFlow (如流) brand mark. lucide-react ships
// no brand icons, so every channel carries its own `*-mark.tsx` (see
// IntegrationChannelIcon): a generic speech-bubble glyph would not tell a
// reader which platform the section belongs to. The mark is drawn as a rounded
// chat bubble with the InfoFlow "flow" stroke and inherits `currentColor` so it
// tracks the surrounding text color in both themes.
export function InfoFlowMark({ className }: { className?: string }) {
  return (
    <svg
      viewBox="0 0 48 48"
      aria-hidden="true"
      data-testid="infoflow-mark"
      className={className}
      fill="none"
      stroke="currentColor"
      strokeWidth="3.4"
      strokeLinecap="round"
      strokeLinejoin="round"
    >
      <path d="M10 12.5h28a3 3 0 0 1 3 3v17a3 3 0 0 1-3 3H22.5L13 42v-6.5h-3a3 3 0 0 1-3-3v-17a3 3 0 0 1 3-3Z" />
      <path d="M16.5 20.5h9.5" />
      <path d="M16.5 27.5h15" />
      <path d="M30 20.5h1.5" />
    </svg>
  );
}
