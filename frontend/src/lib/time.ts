/** Date relative courte en français (« il y a 5 min »). */
export function relTime(ts: number): string {
  const diff = Date.now() / 1000 - ts;
  if (diff < 60) return "à l'instant";
  if (diff < 3600) return `il y a ${Math.floor(diff / 60)} min`;
  if (diff < 86400) return `il y a ${Math.floor(diff / 3600)} h`;
  return `il y a ${Math.floor(diff / 86400)} j`;
}
