import { clsx, type ClassValue } from "clsx";
import { twMerge } from "tailwind-merge";

/** Conditional class names with Tailwind conflict resolution (shadcn/ui's own helper). */
export function cn(...inputs: ClassValue[]): string {
  return twMerge(clsx(inputs));
}
