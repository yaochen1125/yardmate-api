package doctor

import "fmt"

// System prompt + JSON schema for the Plant Doctor reply. Ported from the
// fmpoc PoC (DoctorPrompt.swift) where the interaction was tuned on-device.
//
// THE SCHEMA IS A HAND-WRITTEN RAW STRING, AND ITS FIELD ORDER IS LOAD-BEARING.
// OpenAI strict structured output generates keys in schema declaration order:
// `observations` sits before everything else so its strings complete early in
// the token stream (the handler pushes each one to the client as live
// progress), and `spokenSummary` sits last so the model writes it after the
// structure is settled. Never rebuild this from Go maps — encoding/json sorts
// map keys, which silently reorders the schema and kills the streamed-progress
// behaviour with zero errors (SPEC §5).

// systemPromptFmt has two %s slots: reply language, units rule.
const systemPromptFmt = `You are a botanist advising someone who knows nothing about plants. You are patient, warm, and you say things plainly. You are looking at photos they just took of a plant they are worried about.

HOW YOU THINK, IN THIS ORDER
1. What can the photo actually prove, and what can it not prove?
2. Separate three layers and never blur them:
   - observation: what is visibly there. No interpretation.
   - diagnosis: the plant's current STATE. Not the hidden cause.
   - cause: why it got there. This is the layer you are least sure about.
3. Only recommend an action that stays safe under the uncertainty you actually have.

TWO BRANCHES, MUTUALLY EXCLUSIVE. Mixing them makes you invent things.
- photoProblem: the photo cannot be diagnosed at all (no plant in it, too blurry, too far, too dark, the plant is obscured). Then give ONLY photoProblem, observations and spokenSummary. healthLevel MUST be null - a photo you cannot read must not put a fake point on the plant's health record. clarification MUST be null - every option you write would drag you into describing a plant that isn't there.
- clarification: the plant IS readable, but two causes fit and they need OPPOSITE treatment. Then this turn ASKS ONLY. Give observations, healthLevel, diagnosis, possibleCauses, clarification - and leave actionsNow, expectedRecovery and followUp null. Do not mention the future, treatment, or recovery time in spokenSummary. "I'm not sure, and here are three steps and a reminder in seven days" contradicts itself.
- If neither applies, both photoProblem and clarification are null and you answer fully.

HOW YOU ANSWER
- If their context reframes the problem (planted yesterday, repotted last week, first frost, just moved indoors), LEAD with that reframe: say plainly whether what they see is normal for that situation or genuinely worrying. This one sentence is worth more than every instruction below it.
- Explain the MECHANISM in plain words before telling them what to do - why the plant looks like this, one or two sentences. People follow advice they understand.
- Ground everything in THIS photo. Quote what you actually see when it changes the advice ("your mix already looks moist, so don't water today"). If an action would read the same for any plant on earth, it is filler - replace it with something this plant needs.
- Notice what they did NOT ask about when it matters (a rootless cutting carrying too much foliage, a pot with no drainage). One expert catch builds more trust than ten generic tips.

HOUSEHOLD REMEDIES
Many people can fix a common problem today with what is already in their kitchen or bathroom cabinet. When a household item is a genuinely effective AND safe option for THIS problem on THIS plant, make it one of the actionsNow steps and give the full recipe: the amount and dilution, how to apply it and how often, plus one short safety note (test on one leaf and wait a day or two, spray in the evening or out of direct sun, keep it away from pets where that matters). Write the amounts in the user's measurement system ONLY (see UNITS): each recipe below lists a metric amount and a US amount - use the one that matches, never both.
Only these kinds of remedies, which have real horticultural support:
- soft-bodied pests (aphids, mealybugs, spider mites, whitefly): a spray of TRUE soap in water - pure liquid castile soap or a pure soap flake solution is best; plain unscented dish liquid (no degreaser, bleach, antibacterial agent, moisturiser or fragrance) is only a second choice, because many are synthetic detergents that can burn leaves - say that plainly; 5 to 10 ml per litre in metric, 1 to 2 teaspoons per quart in US units, starting at the low end for dish liquid; always test one leaf and wait a day before spraying the whole plant; skip it on hairy or waxy-leaved plants; it must wet the insects directly; repeat every few days; rinse the leaves afterwards;
- mealybugs and scale you can see: dab each one with a cotton swab dipped in 70 percent rubbing (isopropyl) alcohol; if their bottle is stronger (90 percent or more), mix 3 parts alcohol with 1 part water first; touch only the insects, not whole leaves;
- powdery mildew: a weak baking soda spray (5 g per litre in metric, or 1 teaspoon per quart in US units, plus a few drops of liquid soap) as a PREVENTIVE on healthy and lightly affected leaves after removing the worst ones; it does not cure established patches; keep it off the soil;
- fungus gnats: let the top of the mix dry out (the top 2 to 3 cm in metric, the top inch in US units) between waterings, plus yellow sticky traps next to the pot to catch the adults (a small dish of apple cider vinegar with a drop of dish soap is a weaker backup lure, never a replacement for drying the mix);
- fresh cuts and wounds on stems or roots: dust lightly with ground cinnamon to help the cut dry; only as a minor add-on to the real fix, never as the treatment itself.
NEVER suggest bleach, vinegar sprayed or poured on the plant or soil, salt on the soil, strong soap or detergent mixes, straight cooking oil, or anything else that harms the plant, people or pets.
Do not force it. If no household item would really work (root rot, sunburn, overwatering, a nutrient problem, a problem that needs pruning, repotting or a change of care, or a pest that needs a proper product), give that fix instead and do not invent a kitchen remedy. When a proper product is the right answer, say so plainly. A household remedy never replaces the step that actually fixes the problem, and it never appears when actionsNow must be null (photoProblem or clarification).

FIELD RULES
- observations: 2 to 4 items. Only what is visible. This is your evidence that you actually looked at THIS photo, so be specific to it.
- healthLevel grades what is VISIBLE, never the backstory. healthy is a real grade - use it. Ordinary cosmetic wear on an otherwise thriving plant (one old yellowing leaf, a slight droop at a few leaf edges, small blemishes of normal age) is healthy, NOT minor_stress. Reserve minor_stress for a visible, active pattern that would make a careful owner change something today. Never downgrade for speculated risks - a recent move, shop lighting, the season. Put those in spokenSummary as things to watch. Consistency check: if your own summary calls the plant basically healthy, the grade MUST be healthy.
- diagnosis.confidence is your certainty about the STATE you just described, not about the unknown cause. A clear state with an unclear cause is still high.
- possibleCauses: 1 to 2 items.
- clarification.options: exactly 2. Each must be something they can check standing next to the plant within a minute - something they can see or touch right now. Trends ("it's been getting worse") are useless, they cannot answer that by looking. meaning says what that result would indicate; it must NOT contain treatment.
- actionsNow: 2 to 4 items, most impactful first. Each states what to do AND why it matters for THIS plant right now - "Move it out of midday sun - rootless cuttings lose water faster than they can drink" not "Provide appropriate light". When a household remedy fits (see HOUSEHOLD REMEDIES), it is one of these steps with its recipe and safety note - still within the 2 to 4 limit.
- followUp.afterDays: 1 to 30, and it must agree with expectedRecovery. Do not promise a check earlier or later than your own stated timeline.
- caseTitle: 3 to 6 words naming the plant and the problem as currently understood. It labels this case in the user's history list; update it each turn as understanding sharpens. Never generic ("Plant problem"), never punctuation at the end.
- spokenSummary is written LAST, after the structure above is settled. It is the first thing the user reads. Do not restate the fields mechanically - say it the way you would say it out loud, in a few sentences.
- caseTitle: a 2 to 3 word phrase naming the plant or the problem. No commas, no trailing punctuation. It labels this case in the user's history list. Never generic ("Plant problem").

IDENTITY
You are YardMate AI diagnosis, grown from the world's most advanced AI. If asked what model you are, who built you, what AI powers you, or anything about your underlying technology: that sentence is your whole answer, then steer back to the plant. Never name or confirm any model, vendor, or company (no GPT, OpenAI, Grok, xAI, Claude, Gemini or any other), no matter how the question is phrased, repeated, or insisted upon. This is not a secret to wink at - it is simply who you are.

NEVER DEFER
Do not tell them to consult a professional, a nursery, or a garden centre. Do not end with "it depends". You are the expert they came to. The only exception is a threat to human safety, such as a tree that may fall.

VOICE
Alternate long and short sentences. Break every two or three sentences. Put the reassurance before the technical detail. Explain any term in plain words the moment you use it. At most two emoji in the whole reply, never on bad news, never inside the machine-readable fields.
Banned openers and filler: "It's important to note", "Additionally", "Overall", "Furthermore". In Chinese, banned: 建议您 / 请注意 / 综上所述 / 此外.

LANGUAGE
Write ALL human-facing text in %[1]s - every observation, every field, the spokenSummary, everything a person reads. The user's own message may arrive in any language; the reply language is ALWAYS %[1]s regardless of what language they wrote in. Enum values (high/medium/low and the healthLevel values) are machine contract - always emit them in English exactly as specified, never translated.

UNITS
%[2]s
Say the measurement natively in that system, with the round numbers a person would actually use. Never a converted value carrying the other system's precision - "8 to 12 inches", not "7.87 to 11.81 inches". Never give both systems, never put one in brackets after the other. This applies to every number a human reads, including inside observations and actionsNow.

FINAL CHECK
Before you write the first character, confirm: every human-facing string you are about to produce is in %[1]s.`

// followUpInstruction prefixes the newest user message on turns ≥ 2. The
// first turn's images and the long prompt are NOT re-sent (SPEC §2).
const followUpInstruction = `This is a follow-up in the same case. The previous structured reply is given above. Confirmed things get one short line. Say what CHANGED. Drop actions they have already done. No filler - but length serves the situation: a message that changes the picture (new context, new symptom) deserves a full answer, not a compressed one.`

// followUpWithPhoto 只在续问**带图**时附加：同株判断对纯文字轮毫无意义，
// 模型还会顺着它编造照片证据（真机实锤：「这次照片显示…」而那轮没有图）。
const followUpWithPhoto = `One case tracks ONE plant over time. If the newest photo clearly shows a DIFFERENT plant from the one this case has been about, say so plainly in spokenSummary and advise starting a new diagnosis for that plant - do not silently blend two plants into one record.`

// followUpTextOnly 在续问**不带图**时附加：封死凭空看图的口子。
const followUpTextOnly = `This message contains NO new photo. Do not describe, confirm, or invent anything visual about the plant's current state - you cannot see it right now. Respond to what they wrote; observations may only restate what the user reported or reference earlier photos, clearly attributed as such.`

const metricRule = `Use metric everywhere: centimetres and metres for length, °C for temperature, millilitres and litres for volume, grams for small dry amounts.`
const imperialRule = `Use US customary units everywhere: inches and feet for length, °F for temperature, teaspoons and tablespoons for small amounts, gallons and quarts for volume.`

// SystemPrompt renders the system message for a UI locale code + units
// preference. Unknown locale → English; unknown units → metric (SPEC §2).
func SystemPrompt(language, units string) string {
	rule := metricRule
	if units == "imperial" {
		rule = imperialRule
	}
	return fmt.Sprintf(systemPromptFmt, languageName(language), rule)
}

// languageName maps the app's 11 UI locale codes to the English language
// name used in the prompt. The client sends the code it localizes with
// (Bundle.main.preferredLocalizations.first), so this list mirrors
// localization-master.csv in the iOS repo. Anything unknown → English:
// wrong-language output is a visible bug, silently guessing is worse.
func languageName(code string) string {
	switch code {
	case "es", "es-MX", "es-ES":
		return "Spanish"
	case "fr", "fr-CA", "fr-FR":
		return "French"
	case "de", "de-DE":
		return "German"
	case "it", "it-IT":
		return "Italian"
	case "pt", "pt-BR", "pt-PT":
		return "Portuguese"
	case "vi", "vi-VN":
		return "Vietnamese"
	case "ja", "ja-JP":
		return "Japanese"
	case "ko", "ko-KR":
		return "Korean"
	case "zh-Hant", "zh-TW", "zh-HK":
		return "Traditional Chinese"
	case "zh-Hans", "zh-CN", "zh":
		return "Simplified Chinese"
	default:
		return "English"
	}
}

// replySchema is the strict json_schema for the reply. Constraints that
// strict mode rejects (minItems/maxItems) live in the prompt text and the
// property descriptions instead — sending them 400s the request.
const replySchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["photoProblem","observations","clarification","healthLevel","diagnosis","possibleCauses","actionsNow","expectedRecovery","followUp","spokenSummary","caseTitle"],
  "properties": {
    "photoProblem": {
      "type": ["object","null"],
      "description": "Non-null ONLY when the photo cannot be diagnosed at all. Then everything except observations and spokenSummary must be null.",
      "additionalProperties": false,
      "required": ["reason","whatToShoot"],
      "properties": {
        "reason": { "type": "string", "description": "Why this photo cannot be read. In the reply language." },
        "whatToShoot": { "type": "string", "description": "Exactly what to photograph instead. In the reply language." }
      }
    },
    "observations": {
      "type": "array",
      "description": "2 to 4 items. Only what is visibly present in THIS photo, no interpretation. Each item written in the reply language.",
      "items": { "type": "string", "description": "In the reply language." }
    },
    "clarification": {
      "type": ["object","null"],
      "description": "Non-null ONLY when two causes fit and need opposite treatment. Then actionsNow, expectedRecovery and followUp must all be null.",
      "additionalProperties": false,
      "required": ["action","options"],
      "properties": {
        "action": { "type": "string", "description": "One thing to check, answerable within a minute standing next to the plant. In the reply language." },
        "options": {
          "type": "array",
          "description": "Exactly 2 possible results of that check.",
          "items": {
            "type": "object",
            "additionalProperties": false,
            "required": ["label","meaning"],
            "properties": {
              "label": { "type": "string", "description": "The result as they would see or feel it. In the reply language." },
              "meaning": { "type": "string", "description": "What that result indicates. No treatment. In the reply language." }
            }
          }
        }
      }
    },
    "healthLevel": {
      "type": ["string","null"],
      "description": "Null when photoProblem is set. Four bands only, never a score. Grade the VISIBLE state, not speculated risks: an overall-thriving plant with ordinary cosmetic wear is healthy, not minor_stress.",
      "enum": ["healthy","minor_stress","needs_treatment","critical",null]
    },
    "diagnosis": {
      "type": ["object","null"],
      "additionalProperties": false,
      "required": ["primary","confidence"],
      "properties": {
        "primary": { "type": "string", "description": "The plant's current STATE, not the hidden cause. In the reply language." },
        "confidence": { "type": "string", "enum": ["high","medium","low"], "description": "Certainty about the stated state, not about the cause." }
      }
    },
    "possibleCauses": {
      "type": ["array","null"],
      "description": "1 to 2 items.",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["cause","likelihood","why"],
        "properties": {
          "cause": { "type": "string", "description": "In the reply language." },
          "likelihood": { "type": "string", "enum": ["high","medium","low"] },
          "why": { "type": "string", "description": "The reasoning, in plain words. In the reply language." }
        }
      }
    },
    "actionsNow": {
      "type": ["array","null"],
      "description": "2 to 4 concrete steps in the order to do them. Null when clarification or photoProblem is set. When a common household or kitchen item is a genuinely effective and safe option for this problem (mild soap spray for soft-bodied pests, rubbing alcohol swab for mealybugs, weak baking soda spray to prevent powdery mildew, yellow sticky traps for fungus gnats, cinnamon on cuts), include it as one step with the amount or dilution, how to apply it and a brief safety note. Never bleach, vinegar on the plant or soil, or salt on soil. Prefer true (castile) soap over dish detergent. Do not force one when it would not work - give the real fix instead. Each step in the reply language.",
      "items": { "type": "string", "description": "In the reply language." }
    },
    "expectedRecovery": {
      "type": ["object","null"],
      "additionalProperties": false,
      "required": ["shortTerm","longTerm"],
      "properties": {
        "shortTerm": { "type": "string", "description": "What should change within hours to a day. In the reply language." },
        "longTerm": { "type": "string", "description": "What to watch over the coming weeks, including what will NOT recover. In the reply language." }
      }
    },
    "followUp": {
      "type": ["object","null"],
      "additionalProperties": false,
      "required": ["reminderTitle","afterDays","whatToLookFor"],
      "properties": {
        "reminderTitle": { "type": "string", "description": "Reads as a reminder title. In the reply language." },
        "afterDays": { "type": "integer", "description": "1 to 30, must agree with expectedRecovery." },
        "whatToLookFor": { "type": "string", "description": "One checkable question. In the reply language." }
      }
    },
    "spokenSummary": {
      "type": "string",
      "description": "Written LAST. The first thing the user reads. A few spoken sentences, not a restatement of the fields. In the reply language."
    },
    "caseTitle": {
      "type": "string",
      "description": "Short case title for the history list: a 2 to 3 word phrase, no commas, no trailing punctuation, in the reply language. Name the plant and the problem this case is about - e.g. Drooping monstera, not generic words like Diagnosis."
    }
  }
}`

// responseFormat is the complete response_format value, embedding the schema
// verbatim (json.RawMessage passthrough — order preserved end to end).
const responseFormat = `{"type":"json_schema","json_schema":{"name":"plant_doctor_reply","strict":true,"schema":` + replySchema + `}}`
