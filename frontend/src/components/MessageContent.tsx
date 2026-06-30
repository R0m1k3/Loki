import { useState } from "react";
import ReactMarkdown, { type Components } from "react-markdown";
import remarkGfm from "remark-gfm";
import { CheckIcon, CopyIcon } from "./Icon";

function copyWithTextarea(value: string) {
  const textarea = document.createElement("textarea");
  textarea.value = value;
  textarea.setAttribute("readonly", "");
  textarea.style.position = "fixed";
  textarea.style.left = "-9999px";
  textarea.style.top = "0";
  document.body.appendChild(textarea);
  textarea.select();
  const ok = document.execCommand("copy");
  textarea.remove();
  if (!ok) throw new Error("copy failed");
}

function CodeBlock({ lang, code }: { lang: string; code: string }) {
  const [copied, setCopied] = useState(false);
  const [failed, setFailed] = useState(false);

  const copy = async () => {
    try {
      if (navigator.clipboard?.writeText) {
        await navigator.clipboard.writeText(code);
      } else {
        copyWithTextarea(code);
      }
      setCopied(true);
      setFailed(false);
      setTimeout(() => setCopied(false), 1500);
    } catch {
      try {
        copyWithTextarea(code);
        setCopied(true);
        setFailed(false);
        setTimeout(() => setCopied(false), 1500);
      } catch {
        setFailed(true);
        setTimeout(() => setFailed(false), 1800);
      }
    }
  };

  return (
    <div className="my-2 overflow-hidden border-[3px] border-line bg-card-deep shadow-hard-sm" style={{ borderRadius: 7 }}>
      <div className="flex items-center justify-between border-b-2 border-chrome-2 px-3 py-1.5">
        <span className="font-mono text-[10.5px] uppercase tracking-wide text-on-dark-2">
          {lang || "code"}
        </span>
        <button
          type="button"
          onClick={copy}
          className="flex items-center gap-1 rounded-md px-1.5 py-0.5 text-[11px] text-on-dark-2 transition-colors hover:text-white"
        >
          {copied ? <CheckIcon size={13} /> : <CopyIcon size={13} />}
          {failed ? "Erreur" : copied ? "Copié" : "Copier"}
        </button>
      </div>
      <pre className="scr m-0 overflow-auto p-3 font-mono text-[12px] leading-relaxed text-on-dark">
        <code>{code}</code>
      </pre>
    </div>
  );
}

const components: Components = {
  pre: ({ children }) => <>{children}</>,
  code({ className, children }) {
    const text = String(children).replace(/\n$/, "");
    const match = /language-(\w+)/.exec(className || "");
    if (match || text.includes("\n")) {
      return <CodeBlock lang={match?.[1] ?? ""} code={text} />;
    }
    return (
      <code className="rounded bg-sunken px-1 py-0.5 font-mono text-[12px] text-accent-1">
        {children}
      </code>
    );
  },
  h1: ({ children }) => (
    <h1 className="mb-2 mt-3 text-[17px] font-bold text-ink first:mt-0">
      {children}
    </h1>
  ),
  h2: ({ children }) => (
    <h2 className="mb-2 mt-3 text-[15px] font-bold text-ink first:mt-0">
      {children}
    </h2>
  ),
  h3: ({ children }) => (
    <h3 className="mb-1.5 mt-2.5 text-[13.5px] font-bold text-ink first:mt-0">
      {children}
    </h3>
  ),
  p: ({ children }) => <p className="my-2 first:mt-0 last:mb-0">{children}</p>,
  ul: ({ children }) => (
    <ul className="my-2 list-disc space-y-1 pl-5 marker:text-muted-3">
      {children}
    </ul>
  ),
  ol: ({ children }) => (
    <ol className="my-2 list-decimal space-y-1 pl-5 marker:text-muted-3">
      {children}
    </ol>
  ),
  li: ({ children }) => <li className="leading-relaxed">{children}</li>,
  a: ({ href, children }) => (
    <a
      href={href}
      target="_blank"
      rel="noreferrer"
      className="text-accent underline decoration-accent/40 underline-offset-2 hover:decoration-accent"
    >
      {children}
    </a>
  ),
  strong: ({ children }) => (
    <strong className="font-semibold text-ink">{children}</strong>
  ),
  em: ({ children }) => <em className="italic">{children}</em>,
  blockquote: ({ children }) => (
    <blockquote className="my-2 border-l-2 border-line-strong pl-3 text-muted">
      {children}
    </blockquote>
  ),
  hr: () => <hr className="my-3 border-line-soft" />,
  table: ({ children }) => (
    <div className="my-2 overflow-x-auto">
      <table className="w-full border-collapse text-[12.5px]">{children}</table>
    </div>
  ),
  th: ({ children }) => (
    <th className="border border-line-strong bg-card-deep px-2.5 py-1.5 text-left font-semibold text-ink-2">
      {children}
    </th>
  ),
  td: ({ children }) => (
    <td className="border border-line-soft px-2.5 py-1.5">{children}</td>
  ),
};

export function MessageContent({ text }: { text: string }) {
  return (
    <ReactMarkdown remarkPlugins={[remarkGfm]} components={components}>
      {text}
    </ReactMarkdown>
  );
}
