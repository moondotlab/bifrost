import i18n, { type BackendModule } from "i18next";
import LanguageDetector from "i18next-browser-languagedetector";
import { createElement, useCallback, type ReactNode } from "react";
import { I18nextProvider, initReactI18next, useTranslation } from "react-i18next";

import { localeToBcp47 } from "./detect";
import { pluralKey, translate, type MessageKey, type TranslateFn } from "./translate";
import type { Locale, MessageParams } from "./types";

import { NAMESPACES, loadCatalog, resources } from "./resources";

export const LOCALE_STORAGE_KEY = "bifrost.locale";

export const SUPPORTED_LOCALES = [
	{ code: "en", label: "English" },
	{ code: "zh-CN", label: "简体中文" },
	{ code: "zh-TW", label: "繁體中文" },
	{ code: "ja", label: "日本語" },
	{ code: "ko", label: "한국어" },
	{ code: "es", label: "Español" },
	{ code: "pt", label: "Português" },
	{ code: "fr", label: "Français" },
	{ code: "de", label: "Deutsch" },
	{ code: "it", label: "Italiano" },
	{ code: "ru", label: "Русский" },
] as const;

export type SupportedLocale = (typeof SUPPORTED_LOCALES)[number]["code"];

export const DEFAULT_LOCALE: SupportedLocale = "en";

const supportedLocaleCodes: readonly string[] = SUPPORTED_LOCALES.map((locale) => locale.code);

export function isSupportedLocale(value: string | null | undefined): value is SupportedLocale {
	return !!value && supportedLocaleCodes.includes(value);
}

export function getLocale(): SupportedLocale {
	const current = i18n.resolvedLanguage ?? i18n.language;
	return isSupportedLocale(current) ? current : DEFAULT_LOCALE;
}

// The language detector persists the choice to localStorage on every change.
export async function changeLocale(locale: SupportedLocale): Promise<SupportedLocale> {
	await i18n.changeLanguage(locale);
	return getLocale();
}

const lazyCatalogBackend: BackendModule = {
	type: "backend",
	init: () => {},
	read: (language, namespace, callback) => {
		loadCatalog(language, namespace).then(
			(catalog) => callback(null, catalog),
			(error: Error) => callback(error, false),
		);
	},
};

// Uses the resolved language, so a failed catalog load does not label English text with the requested language.
function syncDocumentLanguage() {
	if (typeof document !== "undefined") document.documentElement.lang = getLocale();
}

let ready: Promise<unknown> = Promise.resolve();

if (!i18n.isInitialized) {
	i18n.on("languageChanged", syncDocumentLanguage);
	ready = i18n
		.use(lazyCatalogBackend)
		.use(LanguageDetector)
		.use(initReactI18next)
		.init({
			resources,
			// English is bundled; other languages come from lazyCatalogBackend.
			partialBundledLanguages: true,
			ns: [...NAMESPACES],
			defaultNS: "common",
			// Namespaces are chosen via useTranslation(ns) or { ns }, never a "ns:key" prefix.
			// Keep in sync with CustomTypeOptions in i18next.d.ts.
			nsSeparator: false,
			fallbackLng: DEFAULT_LOCALE,
			supportedLngs: [...supportedLocaleCodes],
			nonExplicitSupportedLngs: false,
			load: "currentOnly",
			interpolation: {
				escapeValue: false,
			},
			detection: {
				order: ["localStorage", "navigator"],
				lookupLocalStorage: LOCALE_STORAGE_KEY,
				// Keep the fork's saved "zh" preference and normalize Chinese browser locales.
				convertDetectedLanguage: (language: string) => {
					if (/^zh-(?:Hant|TW|HK|MO)(?:-|$)/i.test(language)) return "zh-TW";
					return /^zh(?:-|$)/i.test(language) ? "zh-CN" : language;
				},
				caches: ["localStorage"],
			},
			react: {
				useSuspense: false,
			},
		});
}

// Resolves once the detected language's catalogs are loaded, so the first render is not English.
export const i18nReady = ready.catch(() => undefined);

// Fork components can keep their catalog keys while sharing the active
// language and persistence with components using the upstream namespaces.
export function I18nProvider({ children }: { children: ReactNode }) {
	return createElement(I18nextProvider, { i18n, children });
}

export function useI18n() {
	useTranslation("common", { i18n });
	const active = getLocale();
	const locale: Locale = active.startsWith("zh-") ? "zh" : "en";
	const t = useCallback<TranslateFn>((key, params) => translate(locale, key, params), [locale]);
	const setLocale = useCallback((next: Locale) => {
		void changeLocale(next === "zh" ? "zh-CN" : "en");
	}, []);
	return { locale, bcp47: active === "en" ? localeToBcp47("en") : active, setLocale, t };
}

export function useT(): TranslateFn {
	return useI18n().t;
}

export function usePlural() {
	const t = useT();
	return useCallback((key: MessageKey, count: number, params?: MessageParams) => t(pluralKey(key, count), { count, ...params }), [t]);
}

export { applyDocumentLocale, detectBrowserLocale, localeToBcp47, resolveInitialLocale } from "./detect";
export { translatePageTitle, translateTopbarTitle } from "./titleCatalog";
export { interpolate, pluralKey, translate, type MessageKey, type TranslateFn } from "./translate";
export { LOCALES, LOCALE_META, isLocale, type Locale, type MessageParams } from "./types";
export { NAMESPACES };
export type { I18nNamespace } from "./resources";
export default i18n;