/** Icônes SVG (style line, stroke) reprises du visuel. */
type Props = { size?: number; className?: string };

const base = (size: number, className?: string) => ({
  width: size,
  height: size,
  viewBox: "0 0 24 24",
  fill: "none",
  stroke: "currentColor",
  strokeWidth: 1.7,
  strokeLinecap: "round" as const,
  strokeLinejoin: "round" as const,
  className,
});

export const ChatIcon = ({ size = 19, className }: Props) => (
  <svg {...base(size, className)}>
    <path d="M4 6h16v10H10l-4 3v-3H4z" />
  </svg>
);

export const ClockIcon = ({ size = 19, className }: Props) => (
  <svg {...base(size, className)}>
    <circle cx="12" cy="12" r="8.5" />
    <path d="M12 7.5V12l3 1.8" />
  </svg>
);

export const FilesIcon = ({ size = 19, className }: Props) => (
  <svg {...base(size, className)}>
    <path d="M4 7h6l2 2h8v9H4z" />
  </svg>
);

export const NodesIcon = ({ size = 19, className }: Props) => (
  <svg {...base(size, className)}>
    <circle cx="7" cy="7" r="2.4" />
    <circle cx="17" cy="7" r="2.4" />
    <circle cx="7" cy="17" r="2.4" />
    <circle cx="17" cy="17" r="2.4" />
  </svg>
);

export const SettingsIcon = ({ size = 18, className }: Props) => (
  <svg {...base(size, className)}>
    <line x1="4" y1="8" x2="20" y2="8" />
    <line x1="4" y1="13" x2="20" y2="13" />
    <line x1="4" y1="18" x2="20" y2="18" />
  </svg>
);

export const ChevronDown = ({ size = 13, className }: Props) => (
  <svg {...base(size, className)} strokeWidth={2}>
    <path d="m6 9 6 6 6-6" />
  </svg>
);

export const PlusIcon = ({ size = 13, className }: Props) => (
  <svg {...base(size, className)} strokeWidth={2.2}>
    <path d="M12 5v14M5 12h14" />
  </svg>
);

export const RefreshIcon = ({ size = 15, className }: Props) => (
  <svg {...base(size, className)}>
    <path d="M21 12a9 9 0 1 1-3-6.7M21 4v4h-4" />
  </svg>
);

export const SendIcon = ({ size = 15, className }: Props) => (
  <svg {...base(size, className)} strokeWidth={2.2}>
    <path d="M5 12h14M13 6l6 6-6 6" />
  </svg>
);

export const ClipIcon = ({ size = 16, className }: Props) => (
  <svg {...base(size, className)} strokeWidth={1.8}>
    <path d="M21 11.5 12 20a5 5 0 0 1-7-7l8.5-8.5a3.5 3.5 0 0 1 5 5L10 16" />
  </svg>
);

export const DownloadIcon = ({ size = 14, className }: Props) => (
  <svg {...base(size, className)} strokeWidth={1.9}>
    <path d="M12 4v11m0 0 4-4m-4 4-4-4M5 20h14" />
  </svg>
);

export const CopyIcon = ({ size = 14, className }: Props) => (
  <svg {...base(size, className)} strokeWidth={1.7}>
    <rect x="9" y="9" width="11" height="11" rx="2" />
    <path d="M5 15V5a2 2 0 0 1 2-2h8" />
  </svg>
);

export const CheckIcon = ({ size = 14, className }: Props) => (
  <svg {...base(size, className)} strokeWidth={2.3}>
    <path d="m5 12 4.5 4.5L19 7" />
  </svg>
);

/** Logo Loki : carré orange à bordure blanche + carré interne (néo-brutaliste). */
export const LokiMark = ({ size = 30 }: { size?: number }) => (
  <div
    className="flex items-center justify-center bg-accent"
    style={{
      width: size,
      height: size,
      border: "3px solid #fff",
      boxShadow: "3px 3px 0 #ff5436",
      borderRadius: 7,
    }}
  >
    <div
      style={{ width: size * 0.3, height: size * 0.3, background: "#fff" }}
    />
  </div>
);
